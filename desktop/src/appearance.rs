use serde::{Deserialize, Serialize};
use std::{
    fs::{self, OpenOptions},
    io::{Read, Write},
    path::{Path, PathBuf},
    sync::atomic::{AtomicU64, Ordering},
};
use tauri::{AppHandle, Manager, Theme, Webview};
use url::Url;

const MAX_FILE: u64 = 1024;
static NEXT_TEMP: AtomicU64 = AtomicU64::new(0);

#[derive(Clone, Copy, Debug, Default, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
pub enum Preference {
    #[default]
    System,
    Light,
    Dark,
}

#[derive(Clone, Copy, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
pub enum Resolved {
    Light,
    Dark,
}

#[derive(Clone, Copy, Debug, Serialize, PartialEq, Eq)]
pub struct Appearance {
    preference: Preference,
    resolved: Resolved,
}

#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct Saved {
    format: u8,
    preference: Preference,
}

pub struct Store {
    path: Option<PathBuf>,
    preference: Preference,
    system: Resolved,
    error: Option<String>,
}

impl Default for Store {
    fn default() -> Self {
        Self {
            path: None,
            preference: Preference::System,
            system: Resolved::Dark,
            error: None,
        }
    }
}

impl Store {
    fn load(path: PathBuf) -> Self {
        let mut store = Self {
            path: Some(path.clone()),
            ..Self::default()
        };
        match read_preference(&path) {
            Ok(preference) => store.preference = preference,
            Err(error) => store.error = Some(error),
        }
        store
    }

    fn current(&self) -> Appearance {
        Appearance {
            preference: self.preference,
            resolved: match self.preference {
                Preference::System => self.system,
                Preference::Light => Resolved::Light,
                Preference::Dark => Resolved::Dark,
            },
        }
    }

    fn read(&self) -> Result<Appearance, String> {
        match &self.error {
            Some(error) => Err(error.clone()),
            None => Ok(self.current()),
        }
    }

    fn update(&mut self, preference: Preference) -> Result<Appearance, String> {
        let path = self
            .path
            .as_ref()
            .ok_or("Appearance preferences are unavailable")?;
        save_preference(path, preference)?;
        self.preference = preference;
        self.error = None;
        Ok(self.current())
    }
}

fn read_preference(path: &Path) -> Result<Preference, String> {
    let info = match fs::symlink_metadata(path) {
        Ok(info) => info,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            return Ok(Preference::System)
        }
        Err(_) => return Err("Could not read the saved appearance preference".into()),
    };
    if !info.is_file() || info.len() > MAX_FILE {
        return Err(
            "The saved appearance preference is invalid; choose an appearance to save it again"
                .into(),
        );
    }
    let mut bytes = Vec::new();
    fs::File::open(path)
        .and_then(|file| file.take(MAX_FILE + 1).read_to_end(&mut bytes))
        .map_err(|_| "Could not read the saved appearance preference")?;
    let saved: Saved = serde_json::from_slice(&bytes).map_err(|_| {
        "The saved appearance preference is invalid; choose an appearance to save it again"
    })?;
    if saved.format != 1 || bytes.len() as u64 > MAX_FILE {
        return Err("The saved appearance preference has an unsupported format".into());
    }
    Ok(saved.preference)
}

fn save_preference(path: &Path, preference: Preference) -> Result<(), String> {
    let parent = path
        .parent()
        .ok_or("Appearance preference path is unavailable")?;
    fs::create_dir_all(parent)
        .map_err(|_| "Could not create the appearance preference directory")?;
    match fs::symlink_metadata(path) {
        Ok(info) if !info.is_file() => {
            return Err("Appearance preference must be a regular file".into())
        }
        Err(error) if error.kind() != std::io::ErrorKind::NotFound => {
            return Err("Could not inspect the appearance preference".into())
        }
        _ => {}
    }
    let temp = parent.join(format!(
        ".appearance-{}-{}.tmp",
        std::process::id(),
        NEXT_TEMP.fetch_add(1, Ordering::Relaxed)
    ));
    let result = (|| -> std::io::Result<()> {
        let mut options = OpenOptions::new();
        options.write(true).create_new(true);
        #[cfg(unix)]
        {
            use std::os::unix::fs::OpenOptionsExt;
            options.mode(0o600);
        }
        let mut file = options.open(&temp)?;
        let data = serde_json::to_vec(&Saved {
            format: 1,
            preference,
        })?;
        file.write_all(&data)?;
        file.sync_all()?;
        drop(file);
        fs::rename(&temp, path)?;
        #[cfg(unix)]
        fs::File::open(parent)?.sync_all()?;
        Ok(())
    })();
    let _ = fs::remove_file(&temp);
    result.map_err(|_| "Could not confirm that the appearance preference was saved".into())
}

pub fn initialize(app: &AppHandle) {
    let path = match std::env::var_os("RELAY_DESKTOP_STATE_DIR") {
        Some(path) => {
            let path = PathBuf::from(path);
            if path.is_absolute() {
                Ok(path.join("desktop").join("appearance.json"))
            } else {
                Err("RELAY_DESKTOP_STATE_DIR must be absolute".to_owned())
            }
        }
        None => app
            .path()
            .app_config_dir()
            .map(|path| path.join("appearance.json"))
            .map_err(|_| "Appearance preference directory is unavailable".to_owned()),
    };
    let store = match path {
        Ok(path) => Store::load(path),
        Err(error) => Store {
            error: Some(error),
            ..Store::default()
        },
    };
    if let Ok(mut slot) = app.state::<crate::DesktopState>().appearance.lock() {
        *slot = store;
    }
}

pub fn authorized(label: &str, url: &Url, origin: Option<&Url>) -> bool {
    if crate::frame::trusted_page(label, url) {
        return true;
    }
    label == "main"
        && (crate::launch::is_shell_page(url)
            || origin.is_some_and(|origin| {
                crate::launch::same_origin(origin, url)
                    && url.scheme() == "http"
                    && url.host_str() == Some("127.0.0.1")
                    && url.username().is_empty()
                    && url.password().is_none()
            }))
}

pub fn initialization_script(app: &AppHandle) -> String {
    let state = app.state::<crate::DesktopState>();
    let current = state
        .appearance
        .lock()
        .map(|store| store.current())
        .unwrap_or(Store::default().current());
    let json = serde_json::to_string(&current).expect("appearance consists of fixed enums");
    format!("(()=>{{const state={json};if(state.preference==='system')state.resolved=matchMedia('(prefers-color-scheme: dark)').matches?'dark':'light';window.__RELAY_APPEARANCE__=state;const apply=()=>{{if(!document.documentElement)return false;document.documentElement.dataset.theme=state.resolved;document.documentElement.style.colorScheme=state.resolved;return true;}};if(!apply()){{const observer=new MutationObserver(()=>{{if(apply())observer.disconnect();}});observer.observe(document,{{childList:true,subtree:true}});}}}})();")
}

fn background(resolved: Resolved) -> tauri::window::Color {
    match resolved {
        Resolved::Dark => tauri::window::Color(20, 26, 21, 255),
        Resolved::Light => tauri::window::Color(244, 246, 240, 255),
    }
}

pub fn initial_background(app: &AppHandle) -> tauri::window::Color {
    background(
        app.state::<crate::DesktopState>()
            .appearance
            .lock()
            .map(|store| store.current().resolved)
            .unwrap_or(Resolved::Dark),
    )
}

fn apply_view(view: &Webview, current: Appearance) -> tauri::Result<()> {
    view.set_background_color(Some(background(current.resolved)))?;
    let json = serde_json::to_string(&current).expect("appearance consists of fixed enums");
    view.eval(format!("(()=>{{const state={json};window.__RELAY_APPEARANCE__=state;document.documentElement.dataset.theme=state.resolved;document.documentElement.style.colorScheme=state.resolved;window.dispatchEvent(new CustomEvent('relay-appearance',{{detail:state}}));}})();"))
}

fn broadcast(app: &AppHandle, current: Appearance) -> Result<(), String> {
    if let Some(window) = app.get_window("main") {
        window
            .set_background_color(Some(background(current.resolved)))
            .map_err(|_| "Could not apply the window appearance")?;
    }
    for label in ["main", "chrome", "app-menu"] {
        if let Some(view) = app.get_webview(label) {
            apply_view(&view, current)
                .map_err(|_| "Could not apply appearance to every Relay view")?;
        }
    }
    Ok(())
}

pub fn page_loaded(view: &Webview) {
    let view = view.clone();
    std::thread::spawn(move || {
        let state = view.app_handle().state::<crate::DesktopState>();
        let Ok(_action) = state.appearance_actions.lock() else {
            return;
        };
        let current = state
            .appearance
            .lock()
            .ok()
            .and_then(|store| store.read().ok());
        if let Some(current) = current {
            if let Err(error) = apply_view(&view, current) {
                tracing::error!(%error, "Could not apply Relay appearance");
            }
        }
    });
}

pub fn system_changed(app: &AppHandle, theme: Theme) {
    {
        let state = app.state::<crate::DesktopState>();
        let Ok(mut store) = state.appearance.lock() else {
            return;
        };
        store.system = if theme == Theme::Dark {
            Resolved::Dark
        } else {
            Resolved::Light
        };
    }
    let app = app.clone();
    std::thread::spawn(move || {
        let state = app.state::<crate::DesktopState>();
        let Ok(_action) = state.appearance_actions.lock() else {
            return;
        };
        let current = state
            .appearance
            .lock()
            .ok()
            .and_then(|store| store.read().ok());
        if let Some(current) = current {
            if let Err(error) = broadcast(&app, current) {
                tracing::error!(%error, "Could not update the system appearance");
            }
        }
    });
}

#[tauri::command]
pub async fn appearance(
    webview: Webview,
    preference: Option<Preference>,
) -> Result<Appearance, String> {
    let url = webview.url().map_err(|_| "Appearance is unavailable")?;
    let app = webview.app_handle();
    let state = app.state::<crate::DesktopState>();
    let origin = state
        .origin
        .lock()
        .map_err(|_| "Appearance is unavailable")?;
    if !authorized(webview.label(), &url, origin.as_ref()) {
        return Err("Appearance is only available inside Relay".into());
    }
    drop(origin);
    let _action = state
        .appearance_actions
        .lock()
        .map_err(|_| "Appearance is unavailable")?;
    let current = {
        let mut store = state
            .appearance
            .lock()
            .map_err(|_| "Appearance is unavailable")?;
        match preference {
            Some(preference) => store.update(preference)?,
            None => store.read()?,
        }
    };
    if preference.is_some() {
        broadcast(app, current)?;
    }
    Ok(current)
}

#[cfg(test)]
mod tests {
    use super::*;
    fn temp() -> PathBuf {
        let path = std::env::temp_dir().join(format!(
            "relay-appearance-test-{}-{}",
            std::process::id(),
            NEXT_TEMP.fetch_add(1, Ordering::Relaxed)
        ));
        fs::create_dir(&path).unwrap();
        path
    }
    #[test]
    fn preference_survives_reload_and_replacement() {
        let dir = temp();
        let path = dir.join("appearance.json");
        let mut store = Store::load(path.clone());
        assert_eq!(store.read().unwrap().preference, Preference::System);
        store.update(Preference::Light).unwrap();
        assert_eq!(
            Store::load(path.clone()).read().unwrap().resolved,
            Resolved::Light
        );
        store.update(Preference::Dark).unwrap();
        assert_eq!(
            Store::load(path.clone()).read().unwrap().resolved,
            Resolved::Dark
        );
        store.update(Preference::System).unwrap();
        store.system = Resolved::Light;
        assert_eq!(store.read().unwrap().resolved, Resolved::Light);
        store.system = Resolved::Dark;
        assert_eq!(store.read().unwrap().resolved, Resolved::Dark);
        assert_eq!(fs::read_dir(&dir).unwrap().count(), 1);
        fs::remove_dir_all(dir).unwrap();
    }
    #[test]
    fn corruption_and_save_failure_are_reported_without_false_success() {
        let dir = temp();
        let path = dir.join("appearance.json");
        for bytes in [
            b"{}".to_vec(),
            vec![b'x'; 1025],
            br#"{"format":1,"preference":"light","command":"anything"}"#.to_vec(),
        ] {
            fs::write(&path, bytes).unwrap();
            assert!(Store::load(path.clone()).read().is_err());
        }
        fs::remove_file(&path).unwrap();
        fs::create_dir(&path).unwrap();
        let mut store = Store {
            path: Some(path),
            preference: Preference::Dark,
            ..Store::default()
        };
        assert!(store.update(Preference::Light).is_err());
        assert_eq!(store.current().preference, Preference::Dark);
        fs::remove_dir_all(dir).unwrap();
    }
    #[test]
    fn only_exact_bundled_pages_and_current_controller_can_change_appearance() {
        let origin = Url::parse("http://127.0.0.1:43210").unwrap();
        for (label, url) in [
            ("main", "http://127.0.0.1:43210/"),
            ("main", "tauri://localhost/index.html"),
            ("main", "http://tauri.localhost/error.html"),
            ("chrome", "tauri://localhost/chrome.html"),
            ("app-menu", "http://tauri.localhost/menu.html"),
        ] {
            assert!(
                authorized(label, &Url::parse(url).unwrap(), Some(&origin)),
                "{label} {url}"
            );
        }
        for (label, url) in [
            ("main", "http://127.0.0.1:43211/"),
            ("other", "http://127.0.0.1:43210/"),
            ("chrome", "http://127.0.0.1:43210/"),
            ("main", "http://user@127.0.0.1:43210/"),
            ("main", "http://localhost:43210/"),
            ("main", "tauri://localhost/chrome.html"),
            ("app-menu", "http://tauri.localhost/menu.html?x=1"),
        ] {
            assert!(
                !authorized(label, &Url::parse(url).unwrap(), Some(&origin)),
                "{label} {url}"
            );
        }
        assert!(!authorized("main", &origin, None));
        assert!(serde_json::from_str::<Preference>("\"arbitrary\"").is_err());
    }
}
