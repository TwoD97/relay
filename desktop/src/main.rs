#![cfg_attr(target_os = "windows", windows_subsystem = "windows")]

mod frame;
mod launch;
mod platform;

use launch::{is_shell_page, same_origin, Launcher};
use std::sync::{
    atomic::{AtomicBool, AtomicU64, Ordering},
    Arc, Mutex,
};
use tauri::{
    menu::{Menu, MenuItem, PredefinedMenuItem, Submenu},
    webview::{NewWindowResponse, PageLoadEvent, WebviewBuilder},
    AppHandle, LogicalPosition, LogicalSize, Manager, WebviewUrl, WebviewWindowBuilder,
    WindowEvent,
};
use url::Url;

#[derive(Clone, Default)]
struct DesktopState {
    launcher: Arc<Mutex<Option<Launcher>>>,
    origin: Arc<Mutex<Option<Url>>>,
    launching: Arc<AtomicBool>,
    menu_open: Arc<AtomicBool>,
    menu_generation: Arc<AtomicU64>,
    frame_actions: Arc<Mutex<()>>,
    error_message: Arc<Mutex<Option<String>>>,
}

fn main() {
    tracing_subscriber::fmt()
        .with_target(false)
        .with_writer(std::io::stderr)
        .init();
    if let Err(error) = run() {
        tracing::error!(%error, "Relay desktop could not start");
        std::process::exit(1);
    }
}

fn run() -> tauri::Result<()> {
    let mut builder = tauri::Builder::default();
    // Explicitly isolated controller runs use an isolated WebKit data directory
    // in tests. They must never activate or attach to a user's default window.
    if std::env::var_os("RELAY_DESKTOP_STATE_DIR").is_none() {
        builder = builder.plugin(tauri_plugin_single_instance::init(|app, _, _| {
            if let Some(window) = app.get_window("main") {
                let _ = window.unminimize();
                let _ = window.show();
                let _ = window.set_focus();
            }
        }));
    }
    builder
        .invoke_handler(tauri::generate_handler![frame::frame_action])
        .manage(DesktopState::default())
        .setup(|app| {
            let state = app.state::<DesktopState>().inner().clone();
            let origin = state.origin.clone();
            let error_message = state.error_message.clone();
            let content = WebviewWindowBuilder::new(app, "main", WebviewUrl::App("index.html".into()))
                .title("Relay")
                .inner_size(1280.0, 860.0)
                .min_inner_size(390.0, 500.0)
                .decorations(false)
                .visible(false)
                .background_color(tauri::window::Color(16, 19, 18, 255))
                // Only this webview can reach the authenticated controller,
                // with navigation constrained below. Enable its standard web
                // clipboard API for explicit copy/paste gestures; no native
                // command or clipboard plugin is exposed to page scripts.
                .enable_clipboard_access()
                    .on_navigation(move |url| {
                        is_shell_page(url)
                            || origin
                                .lock()
                                .ok()
                                .and_then(|origin| {
                                    origin.as_ref().map(|address| same_origin(address, url))
                                })
                                .unwrap_or(false)
                    })
                    .on_page_load(move |webview, payload| {
                        if payload.event() != PageLoadEvent::Finished
                            || !is_shell_page(payload.url())
                            || payload.url().path() != "/error.html"
                        {
                            return;
                        }
                        let message = error_message.lock().ok().and_then(|message| message.clone());
                        if let Some(message) = message {
                            let expected_url = serde_json::json!(payload.url().as_str());
                            let message = serde_json::json!(message);
                            let script = format!("if(location.href === {expected_url}) {{ const status = document.querySelector('[role=\"status\"]'); if(status) status.textContent = {message}; }}");
                            if webview.eval(&script).is_err() {
                                tracing::error!("Could not display the controller diagnostic.");
                            }
                        }
                    })
                    .on_new_window(|_, _| NewWindowResponse::Deny)
                    .devtools(false)
                    .build()?;
            let window = app.get_window("main").expect("created main window");
            content.as_ref().set_auto_resize(false)?;
            window.add_child(
                WebviewBuilder::new("chrome", WebviewUrl::App("chrome.html".into()))
                    .background_color(tauri::window::Color(16, 19, 18, 255))
                    .on_navigation(|url| frame::trusted_page("chrome", url))
                    .on_new_window(|_, _| NewWindowResponse::Deny)
                    .on_page_load(|webview, payload| {
                        if payload.event() == PageLoadEvent::Finished {
                            frame::update_state(&webview.window());
                        }
                    })
                    .devtools(false),
                LogicalPosition::new(0.0, 0.0),
                LogicalSize::new(1280.0, frame::HEIGHT),
            )?;
            let menu_view = window.add_child(
                WebviewBuilder::new("app-menu", WebviewUrl::App("menu.html".into()))
                    .background_color(tauri::window::Color(23, 28, 25, 255))
                    .on_navigation(|url| frame::trusted_page("app-menu", url))
                    .on_new_window(|_, _| NewWindowResponse::Deny)
                    .devtools(false),
                LogicalPosition::new(8.0, frame::HEIGHT + 4.0),
                LogicalSize::new(frame::MENU_WIDTH, frame::MENU_HEIGHT),
            )?;
            platform::configure_frame(&window)?;
            menu_view.hide()?;
            let frame_window = window.clone();
            window.on_window_event(move |event| match event {
                WindowEvent::Resized(_) | WindowEvent::ScaleFactorChanged { .. } => {
                    frame::layout(&frame_window);
                }
                WindowEvent::Focused(_) => {
                    frame::update_state(&frame_window);
                    // Child webview focus transfers can transiently report
                    // false here. The menu handles settled DOM focus loss.
                }
                _ => {}
            });
            let reload = MenuItem::with_id(app, "reload", "Reload", true, Some("CmdOrCtrl+R"))?;
            let reconnect = MenuItem::with_id(
                app,
                "reconnect",
                "Reconnect",
                true,
                Some("CmdOrCtrl+Shift+R"),
            )?;
            let browser = MenuItem::with_id(
                app,
                "browser",
                "Open in Browser",
                true,
                Some("CmdOrCtrl+Shift+B"),
            )?;
            let quit = MenuItem::with_id(app, "quit", "Quit Relay", true, Some("CmdOrCtrl+Q"))?;
            let separator = PredefinedMenuItem::separator(app)?;
            let relay = Submenu::with_items(
                app,
                "Relay",
                true,
                &[&reload, &reconnect, &browser, &separator, &quit],
            )?;
            window.set_menu(Menu::with_items(app, &[&relay])?)?;
            // Keep native accelerators while drawing all visible chrome ourselves.
            window.hide_menu()?;
            frame::layout(&window);
            match app
                .path()
                .resource_dir()
                .map_err(|error| error.to_string())
                .and_then(|resources| {
                    Launcher::from_environment(&resources).map_err(|error| error.to_string())
                }) {
                Ok(launcher) => {
                    if let Ok(mut slot) = state.launcher.lock() {
                        *slot = Some(launcher);
                    }
                    connect(app.handle().clone(), false);
                }
                Err(error) => {
                    tracing::error!(%error, "Relay resources are unavailable");
                    show_connection_error(app.handle(), &error);
                }
            }
            window.show()?;
            Ok(())
        })
        .on_menu_event(|app, event| match event.id().as_ref() {
            "reload" => app_action(app, frame::Action::Reload),
            "reconnect" => app_action(app, frame::Action::Reconnect),
            "browser" => app_action(app, frame::Action::Browser),
            "quit" => app.exit(0),
            _ => {}
        })
        // The controller view has no native capabilities. Only immutable bundled
        // chrome can invoke the small, independently checked frame command.
        // Closing the window leaves controllers and terminal sessions running.
        .run(tauri::generate_context!())
}

fn app_action(app: &AppHandle, action: frame::Action) {
    match action {
        frame::Action::Reload => {
            if let Some(content) = app.get_webview("main") {
                if content.reload().is_err() {
                    tracing::error!("Could not reload Relay");
                }
            }
        }
        frame::Action::Reconnect => connect(app.clone(), false),
        frame::Action::Browser => connect(app.clone(), true),
        _ => {}
    }
}

fn connect(app: AppHandle, browser: bool) {
    let state = app.state::<DesktopState>().inner().clone();
    if state.launching.swap(true, Ordering::AcqRel) {
        return;
    }
    std::thread::spawn(move || {
        let launcher = state
            .launcher
            .lock()
            .ok()
            .and_then(|launcher| launcher.clone());
        let result = launcher
            .ok_or_else(|| {
                launch::LaunchError::new(
                    "The Relay controller resource is unavailable. Reinstall the desktop app.",
                )
            })
            .and_then(|launcher| launcher.launch());
        match result {
            Ok(launch) if browser => {
                // Only a freshly validated HTTP loopback sign-in URL can reach
                // the native opener, and only through this explicit menu action.
                if open::that_detached(launch.url.as_str()).is_err() {
                    tracing::error!("Could not open the system browser. Check that a default browser is configured.");
                    show_connection_error(&app, "Could not open your browser. Set a default browser in system settings, then choose Relay → Reconnect to return to your fleet.");
                }
            }
            Ok(launch) => {
                if let Ok(mut message) = state.error_message.lock() {
                    *message = None;
                }
                if let Ok(mut origin) = state.origin.lock() {
                    *origin = Some(launch.address);
                }
                if let Some(window) = app.get_webview("main") {
                    if window.navigate(launch.url).is_err() {
                        tracing::error!("Could not navigate to the local Relay controller.");
                        show_connection_error(&app, "Could not open the local Relay controller. Choose Relay → Reconnect to try again.");
                    }
                }
            }
            Err(error) => {
                tracing::error!(%error, "Relay connection failed");
                show_connection_error(&app, &error.to_string());
            }
        }
        state.launching.store(false, Ordering::Release);
    });
}

fn show_connection_error(app: &AppHandle, message: &str) {
    if let Ok(mut error) = app.state::<DesktopState>().error_message.lock() {
        *error = Some(message.to_owned());
    }
    if let (Some(window), Ok(url)) = (
        app.get_webview("main"),
        Url::parse(platform::shell_error_url()),
    ) {
        if window.navigate(url).is_err() {
            tracing::error!("Could not display the Relay connection error page.");
        }
    }
}
