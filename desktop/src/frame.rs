use std::sync::atomic::Ordering;
use tauri::{AppHandle, Manager, PhysicalPosition, PhysicalSize, Rect, Webview, Window};
use url::Url;

pub const HEIGHT: f64 = 40.0;
pub const MENU_WIDTH: f64 = 284.0;
pub const MENU_HEIGHT: f64 = 197.0;

#[derive(Clone, Copy, Debug, serde::Deserialize)]
#[serde(rename_all = "kebab-case")]
pub enum Action {
    Drag,
    Minimize,
    Maximize,
    Close,
    Menu,
    DismissMenu,
    CheckMenuFocus,
    CheckChromeFocus,
    ConfirmMenuBlur,
    EscapeMenu,
    Reload,
    Reconnect,
}

pub fn trusted_page(label: &str, url: &Url) -> bool {
    let path = match label {
        "chrome" => "/chrome.html",
        "app-menu" => "/menu.html",
        _ => return false,
    };
    matches!(
        (url.scheme(), url.host_str()),
        ("tauri", Some("localhost")) | ("http", Some("tauri.localhost"))
    ) && url.path() == path
        && url.query().is_none()
        && url.fragment().is_none()
        && url.username().is_empty()
        && url.password().is_none()
        && url.port().is_none()
}

fn bounds(size: PhysicalSize<u32>, scale: f64) -> Option<[Rect; 3]> {
    if size.width == 0 || size.height < 2 || !scale.is_finite() || scale <= 0.0 {
        return None;
    }
    let header = (HEIGHT * scale)
        .round()
        .clamp(1.0, (size.height - 1) as f64) as u32;
    // Partition physical pixels once. Passing logical bounds back to Wry would
    // convert them using each child HWND's DPI, which may still be changing.
    Some([
        Rect {
            position: PhysicalPosition::new(0.0, header as f64).into(),
            size: PhysicalSize::new(size.width, size.height - header).into(),
        },
        Rect {
            position: PhysicalPosition::new(0.0, 0.0).into(),
            size: PhysicalSize::new(size.width, header).into(),
        },
        Rect {
            position: PhysicalPosition::new(
                (8.0 * scale).round(),
                header as f64 + (4.0 * scale).round(),
            )
            .into(),
            size: PhysicalSize::new((MENU_WIDTH * scale).round(), (MENU_HEIGHT * scale).round())
                .into(),
        },
    ])
}

pub fn layout(window: &Window) {
    let (Ok(size), Ok(scale)) = (window.inner_size(), window.scale_factor()) else {
        return;
    };
    layout_at(window, size, scale);
}

pub fn layout_at(window: &Window, size: PhysicalSize<u32>, scale: f64) {
    let Some(bounds) = bounds(size, scale) else {
        return;
    };
    for (label, bounds) in ["main", "chrome", "app-menu"].into_iter().zip(bounds) {
        if let Some(view) = window.app_handle().get_webview(label) {
            let _ = view.set_bounds(bounds);
        }
    }
    update_state(window);
}

pub fn update_state(window: &Window) {
    if let Some(chrome) = window.app_handle().get_webview("chrome") {
        let maximized = window.is_maximized().unwrap_or(false);
        let focused = window.is_focused().unwrap_or(true);
        let _ = chrome.eval(format!(
            "window.dispatchEvent(new CustomEvent('relay-frame-state',{{detail:{{maximized:{maximized},focused:{focused}}}}}));"
        ));
    }
}

pub fn dismiss_menu(app: &AppHandle, focus_content: bool) {
    let state = app.state::<crate::DesktopState>();
    state.menu_generation.fetch_add(1, Ordering::AcqRel);
    state.menu_open.store(false, Ordering::Release);
    if let Some(menu) = app.get_webview("app-menu") {
        let _ = menu.hide();
    }
    if let Some(chrome) = app.get_webview("chrome") {
        let _ = chrome.eval(
            "document.getElementById('app-menu-toggle')?.setAttribute('aria-expanded','false');",
        );
    }
    if focus_content {
        if let Some(content) = app.get_webview("main") {
            let _ = content.set_focus();
        }
    }
}

fn matches_menu_generation(state: &crate::DesktopState, generation: Option<u64>) -> bool {
    state.menu_open.load(Ordering::Acquire)
        && generation == Some(state.menu_generation.load(Ordering::Acquire))
}

#[tauri::command]
pub async fn frame_action(
    webview: Webview,
    action: Action,
    generation: Option<u64>,
) -> Result<(), String> {
    let url = webview.url().map_err(|_| "Frame unavailable")?;
    // ACL is restricted to these local webviews as well. Keep the independent
    // native boundary: content, another label, and navigated chrome all fail.
    if !trusted_page(webview.label(), &url) {
        return Err("Desktop actions are only available from the trusted Relay frame".into());
    }
    let app = webview.app_handle();
    // Stay off the IPC thread: WebView operations can await main-thread
    // dispatch. Serialize each short action so show/focus/hide sequences from
    // separate async command tasks cannot interleave. No await holds this lock.
    let state = app.state::<crate::DesktopState>();
    let _action = state
        .frame_actions
        .lock()
        .map_err(|_| "Desktop actions are unavailable")?;
    let window = app.get_window("main").ok_or("Window unavailable")?;
    let result = match action {
        Action::Drag => {
            dismiss_menu(app, false);
            window.start_dragging()
        }
        Action::Minimize => {
            dismiss_menu(app, false);
            window.minimize()
        }
        Action::Maximize => {
            dismiss_menu(app, false);
            if window.is_maximized().unwrap_or(false) {
                window.unmaximize()
            } else {
                window.maximize()
            }
        }
        Action::Close => window.close(),
        Action::Menu => {
            if app
                .state::<crate::DesktopState>()
                .menu_open
                .swap(true, Ordering::AcqRel)
            {
                dismiss_menu(app, false);
                return Ok(());
            }
            state.menu_generation.fetch_add(1, Ordering::AcqRel);
            if let Some(menu) = app.get_webview("app-menu") {
                if menu.show().and_then(|_| menu.set_focus()).is_err() {
                    dismiss_menu(app, false);
                    return Err("Could not open Relay menu".into());
                }
                let _ = menu.eval("window.dispatchEvent(new Event('relay-menu-open')); document.querySelector('button')?.focus();");
            }
            if let Some(chrome) = app.get_webview("chrome") {
                let _ = chrome.eval("document.getElementById('app-menu-toggle')?.setAttribute('aria-expanded','true');");
            }
            Ok(())
        }
        Action::DismissMenu => {
            if matches_menu_generation(&state, generation) {
                dismiss_menu(app, false);
            }
            Ok(())
        }
        Action::CheckMenuFocus | Action::CheckChromeFocus | Action::ConfirmMenuBlur => {
            if !state.menu_open.load(Ordering::Acquire)
                || (!matches!(action, Action::CheckMenuFocus)
                    && !matches_menu_generation(&state, generation))
            {
                return Ok(());
            }
            let generation = state.menu_generation.load(Ordering::Acquire);
            let (label, event) = match action {
                Action::CheckMenuFocus => ("app-menu", "relay-menu-check-focus"),
                Action::CheckChromeFocus => ("chrome", "relay-chrome-check-focus"),
                _ => ("app-menu", "relay-menu-confirm-blur"),
            };
            if let Some(view) = app.get_webview(label) {
                let _ = view.eval(format!(
                    "window.dispatchEvent(new CustomEvent('{event}',{{detail:{{generation:{generation}}}}}));"
                ));
            }
            Ok(())
        }
        Action::EscapeMenu => {
            dismiss_menu(app, false);
            if let Some(chrome) = app.get_webview("chrome") {
                let _ = chrome.set_focus();
                let _ = chrome.eval("document.getElementById('app-menu-toggle')?.focus();");
            }
            Ok(())
        }
        Action::Reload | Action::Reconnect => {
            dismiss_menu(app, true);
            crate::app_action(app, action);
            Ok(())
        }
    };
    result.map_err(|_| "The desktop action could not be completed".into())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn controller_and_other_webviews_cannot_operate_the_frame() {
        for candidate in [
            "http://127.0.0.1:7340/chrome.html",
            "https://tauri.localhost/chrome.html",
            "http://tauri.localhost:80/chrome.html?x=1",
            "http://tauri.localhost/chrome.html#x",
            "tauri://localhost/index.html",
            "tauri://localhost/menu.html",
            "tauri://user@localhost/chrome.html",
            "tauri://localhost/chrome.html/",
        ] {
            assert!(!trusted_page("chrome", &Url::parse(candidate).unwrap()));
        }
        let chrome = Url::parse("tauri://localhost/chrome.html").unwrap();
        assert!(!trusted_page("main", &chrome));
        assert!(!trusted_page("app-menu", &chrome));
        assert!(trusted_page("chrome", &chrome));
        assert!(trusted_page(
            "app-menu",
            &Url::parse("http://tauri.localhost/menu.html").unwrap()
        ));
    }

    #[test]
    fn frame_layout_partitions_every_physical_pixel_at_fractional_dpi() {
        for scale in [1.0, 1.1, 1.25, 1.5, 1.75, 2.0, 2.25, 3.0] {
            for size in [
                PhysicalSize::new(1281, 861),
                PhysicalSize::new(977, 703),
                PhysicalSize::new(391, 501),
            ] {
                let [content, chrome, menu] = bounds(size, scale).unwrap();
                // Physical bounds must not be rescaled even if a child still
                // reports the previous monitor's DPI during this event.
                for child_scale in [1.0, 1.25, 2.0] {
                    let content_size = content.size.to_physical::<u32>(child_scale);
                    let content_pos = content.position.to_physical::<i32>(child_scale);
                    let chrome_size = chrome.size.to_physical::<u32>(child_scale);
                    assert_eq!(content_size.width, size.width);
                    assert_eq!(chrome_size.width, size.width);
                    assert_eq!(content_pos.x, 0);
                    assert_eq!(content_pos.y as u32, chrome_size.height);
                    assert_eq!(chrome_size.height + content_size.height, size.height);
                    assert_eq!(chrome_size.height, (HEIGHT * scale).round() as u32);
                    assert_eq!(
                        menu.position.to_physical::<i32>(child_scale).y,
                        content_pos.y + (4.0 * scale).round() as i32
                    );
                }
            }
        }
    }

    #[test]
    fn dpi_transition_uses_event_dimensions_and_ignores_minimized_sizes() {
        let old = PhysicalSize::new(1280, 860);
        let new = PhysicalSize::new(1920, 1290);
        let [content, chrome, _] = bounds(new, 1.5).unwrap();
        assert_eq!(
            content.size.to_physical::<u32>(1.0),
            PhysicalSize::new(1920, 1230)
        );
        assert_eq!(
            chrome.size.to_physical::<u32>(1.0),
            PhysicalSize::new(1920, 60)
        );
        assert_ne!(content.size.to_physical::<u32>(1.0).width, old.width);
        for size in [
            PhysicalSize::new(0, 860),
            PhysicalSize::new(1280, 0),
            PhysicalSize::new(1280, 1),
        ] {
            assert!(bounds(size, 1.0).is_none());
        }
        for scale in [0.0, -1.0, f64::NAN, f64::INFINITY] {
            assert!(bounds(old, scale).is_none());
        }
    }

    #[test]
    fn frame_protocol_has_no_arbitrary_url_or_execution_action() {
        for action in [
            "browser",
            "open-url",
            "execute",
            "navigate",
            "open-devtools",
        ] {
            assert!(serde_json::from_str::<Action>(&format!("\"{action}\"")).is_err());
        }
    }

    #[test]
    fn stale_blur_checks_cannot_dismiss_a_reopened_menu() {
        let state = crate::DesktopState::default();
        state.menu_open.store(true, Ordering::Release);
        state.menu_generation.store(1, Ordering::Release);
        assert!(matches_menu_generation(&state, Some(1)));
        assert!(!matches_menu_generation(&state, None));

        state.menu_open.store(false, Ordering::Release);
        state.menu_generation.fetch_add(1, Ordering::AcqRel);
        assert!(!matches_menu_generation(&state, Some(1)));
        assert!(!matches_menu_generation(&state, Some(2)));

        state.menu_open.store(true, Ordering::Release);
        state.menu_generation.fetch_add(1, Ordering::AcqRel);
        assert!(!matches_menu_generation(&state, Some(1)));
        assert!(!matches_menu_generation(&state, Some(2)));
        assert!(matches_menu_generation(&state, Some(3)));
    }
}
