use serde::{Deserialize, Serialize};
use std::{
    fs::{self, File, OpenOptions},
    io::{Read, Write},
    path::{Path, PathBuf},
    sync::atomic::{AtomicU64, Ordering},
};
use tauri::{AppHandle, Manager, Webview};
use url::Url;

const MAX_FILE: u64 = 1024;
static NEXT_TEMP: AtomicU64 = AtomicU64::new(0);

#[derive(Clone, Copy, Debug, Default, Deserialize, Serialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
pub struct SidebarAttention {
    collapsed: bool,
}

#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct Saved {
    format: u8,
    collapsed: bool,
}

#[derive(Default)]
pub struct Store {
    path: Option<PathBuf>,
    current: SidebarAttention,
    error: Option<String>,
}

impl Store {
    fn load(path: PathBuf) -> Self {
        let mut store = Self {
            path: Some(path),
            ..Self::default()
        };
        let _ = store.read();
        store
    }

    fn read(&mut self) -> Result<SidebarAttention, String> {
        let path = self.path.as_ref().ok_or_else(|| {
            self.error
                .clone()
                .unwrap_or_else(|| "Sidebar preferences are unavailable".into())
        })?;
        // Detached windows are separate processes. Read the shared preference
        // rather than returning a stale process-local copy after another saves.
        match read_saved(path) {
            Ok(value) => {
                self.current = value;
                self.error = None;
                Ok(value)
            }
            Err(error) => {
                self.error = Some(error.clone());
                Err(error)
            }
        }
    }

    fn update(&mut self, collapsed: bool) -> Result<SidebarAttention, String> {
        let path = self
            .path
            .as_ref()
            .ok_or("Sidebar preferences are unavailable")?;
        save(path, collapsed)?;
        self.current = SidebarAttention { collapsed };
        self.error = None;
        Ok(self.current)
    }
}

fn linked(info: &fs::Metadata) -> bool {
    #[cfg(windows)]
    {
        use std::os::windows::fs::MetadataExt;
        info.file_attributes() & 0x400 != 0
    }
    #[cfg(not(windows))]
    {
        info.file_type().is_symlink()
    }
}

fn checked_file(path: &Path) -> Result<Option<File>, String> {
    let info = match fs::symlink_metadata(path) {
        Ok(info) => info,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(_) => return Err("Could not read the saved sidebar preference".into()),
    };
    if !info.is_file() || linked(&info) || info.len() > MAX_FILE {
        return Err(
            "The saved sidebar preference is invalid; toggle Needs attention to save it again"
                .into(),
        );
    }
    let mut options = OpenOptions::new();
    options.read(true);
    #[cfg(target_os = "linux")]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.custom_flags(0x20000); // O_NOFOLLOW; never read a replaced symlink.
    }
    #[cfg(windows)]
    {
        use std::os::windows::fs::OpenOptionsExt;
        options.custom_flags(0x0020_0000); // FILE_FLAG_OPEN_REPARSE_POINT.
    }
    let file = options
        .open(path)
        .map_err(|_| "Could not read the saved sidebar preference")?;
    let info = file
        .metadata()
        .map_err(|_| "Could not inspect the saved sidebar preference")?;
    if !info.is_file() || linked(&info) || info.len() > MAX_FILE {
        return Err("The saved sidebar preference must be a bounded regular file".into());
    }
    Ok(Some(file))
}

fn read_saved(path: &Path) -> Result<SidebarAttention, String> {
    let Some(file) = checked_file(path)? else {
        return Ok(SidebarAttention::default());
    };
    let mut bytes = Vec::new();
    file.take(MAX_FILE + 1)
        .read_to_end(&mut bytes)
        .map_err(|_| "Could not read the saved sidebar preference")?;
    if bytes.len() as u64 > MAX_FILE {
        return Err("The saved sidebar preference is too large".into());
    }
    let saved: Saved = serde_json::from_slice(&bytes).map_err(|_| {
        "The saved sidebar preference is invalid; toggle Needs attention to save it again"
    })?;
    if saved.format != 1 {
        return Err("The saved sidebar preference has an unsupported format".into());
    }
    Ok(SidebarAttention {
        collapsed: saved.collapsed,
    })
}

fn save(path: &Path, collapsed: bool) -> Result<(), String> {
    let parent = path
        .parent()
        .ok_or("Sidebar preference directory is unavailable")?;
    let mut directory = fs::DirBuilder::new();
    directory.recursive(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::DirBuilderExt;
        directory.mode(0o700);
    }
    // Windows inherits the current user's private app-config/controller-state
    // directory ACL. No machine-wide or shared directory is used by default.
    directory
        .create(parent)
        .map_err(|_| "Could not create the sidebar preference directory")?;
    // Preserve linked/nonregular targets. An explicit user write can replace a
    // malformed regular JSON preference; a read never repairs it silently.
    match fs::symlink_metadata(path) {
        Ok(info) if !info.is_file() || linked(&info) => {
            return Err("Sidebar preference must be a regular file".into())
        }
        Err(error) if error.kind() != std::io::ErrorKind::NotFound => {
            return Err("Could not inspect the sidebar preference".into())
        }
        _ => {}
    }
    let temp = parent.join(format!(
        ".sidebar-{}-{}.tmp",
        std::process::id(),
        NEXT_TEMP.fetch_add(1, Ordering::Relaxed)
    ));
    let mut options = OpenOptions::new();
    options.write(true).create_new(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.mode(0o600);
    }
    // If exclusive creation fails, never remove an entry we did not create.
    let mut file = options
        .open(&temp)
        .map_err(|_| "Could not create a private sidebar preference")?;
    let result = (|| -> std::io::Result<()> {
        file.write_all(&serde_json::to_vec(&Saved {
            format: 1,
            collapsed,
        })?)?;
        file.sync_all()?;
        drop(file);
        fs::rename(&temp, path)?;
        #[cfg(unix)]
        File::open(parent)?.sync_all()?;
        Ok(())
    })();
    let _ = fs::remove_file(temp);
    result.map_err(|_| "Could not confirm that the sidebar preference was saved".into())
}

pub fn initialize(app: &AppHandle) {
    let path = match std::env::var_os("RELAY_DESKTOP_STATE_DIR") {
        Some(value) => {
            let path = PathBuf::from(value);
            if path.is_absolute() {
                Ok(path.join("desktop").join("sidebar.json"))
            } else {
                Err("RELAY_DESKTOP_STATE_DIR must be absolute".to_owned())
            }
        }
        None => app
            .path()
            .app_config_dir()
            .map(|path| path.join("sidebar.json"))
            .map_err(|_| "Sidebar preference directory is unavailable".to_owned()),
    };
    let store = match path {
        Ok(path) => Store::load(path),
        Err(error) => Store {
            error: Some(error),
            ..Store::default()
        },
    };
    if let Ok(mut slot) = app.state::<crate::DesktopState>().sidebar.lock() {
        *slot = store;
    }
}

pub fn initialization_script(app: &AppHandle) -> String {
    let current = app
        .state::<crate::DesktopState>()
        .sidebar
        .lock()
        .map(|store| store.current)
        .unwrap_or_default();
    format!(
        "window.__RELAY_SIDEBAR_ATTENTION__={};",
        serde_json::to_string(&current).expect("sidebar preference contains only a boolean")
    )
}

pub fn authorized(label: &str, url: &Url, origin: Option<&Url>) -> bool {
    crate::session_window::authorized(label, url, origin)
}

#[tauri::command]
pub async fn sidebar_attention(
    webview: Webview,
    collapsed: Option<bool>,
) -> Result<SidebarAttention, String> {
    let url = webview
        .url()
        .map_err(|_| "Sidebar preferences are unavailable")?;
    let state = webview.app_handle().state::<crate::DesktopState>();
    let origin = state
        .origin
        .lock()
        .map_err(|_| "Sidebar preferences are unavailable")?;
    if !authorized(webview.label(), &url, origin.as_ref()) {
        return Err(
            "Sidebar preferences are only available in the current Relay application".into(),
        );
    }
    drop(origin);
    let mut store = state
        .sidebar
        .lock()
        .map_err(|_| "Sidebar preferences are unavailable")?;
    match collapsed {
        Some(value) => store.update(value),
        None => store.read(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    fn temp() -> PathBuf {
        let path = std::env::temp_dir().join(format!(
            "relay-sidebar-test-{}-{}",
            std::process::id(),
            NEXT_TEMP.fetch_add(1, Ordering::Relaxed)
        ));
        fs::create_dir(&path).unwrap();
        path
    }
    #[test]
    fn saved_preference_survives_reopen_and_independent_readers() {
        let dir = temp();
        let path = dir.join("sidebar.json");
        let mut first = Store::load(path.clone());
        let mut other = Store::load(path.clone());
        assert!(!first.read().unwrap().collapsed);
        assert!(first.update(true).unwrap().collapsed);
        assert!(Store::load(path.clone()).read().unwrap().collapsed);
        assert!(other.read().unwrap().collapsed);
        other.update(false).unwrap();
        assert!(!first.read().unwrap().collapsed);
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            assert_eq!(
                fs::metadata(&path).unwrap().permissions().mode() & 0o777,
                0o600
            );
        }
        assert_eq!(fs::read_dir(&dir).unwrap().count(), 1);
        fs::remove_dir_all(dir).unwrap();
    }
    #[test]
    fn invalid_saved_data_is_preserved_until_an_explicit_write() {
        let dir = temp();
        let path = dir.join("sidebar.json");
        for bytes in [
            b"{}".to_vec(),
            vec![b'x'; 1025],
            br#"{"format":1,"collapsed":true,"command":"anything"}"#.to_vec(),
            br#"{"format":2,"collapsed":true}"#.to_vec(),
            br#"{"format":1,"collapsed":"true"}"#.to_vec(),
        ] {
            fs::write(&path, &bytes).unwrap();
            let mut store = Store::load(path.clone());
            assert!(store.read().is_err());
            assert!(!store.current.collapsed);
            assert_eq!(fs::read(&path).unwrap(), bytes);
        }
        let mut store = Store::load(path.clone());
        store.update(true).unwrap();
        assert!(store.read().unwrap().collapsed);
        fs::remove_file(&path).unwrap();
        fs::create_dir(&path).unwrap();
        assert!(store.update(false).is_err());
        assert!(store.current.collapsed);
        fs::remove_dir_all(dir).unwrap();
    }
    #[cfg(unix)]
    #[test]
    fn linked_targets_are_never_read_or_replaced() {
        use std::os::unix::fs::symlink;
        let dir = temp();
        let real = dir.join("real");
        fs::create_dir(&real).unwrap();
        let target = real.join("sidebar.json");
        fs::write(&target, b"keep").unwrap();
        let link = dir.join("sidebar.json");
        symlink(&target, &link).unwrap();
        assert!(read_saved(&link).is_err());
        assert!(save(&link, true).is_err());
        assert_eq!(fs::read(&target).unwrap(), b"keep");
        // A redirected profile is valid; only the preference leaf is forbidden
        // from redirecting reads or replacements to another file.
        let parent = dir.join("linked");
        symlink(&real, &parent).unwrap();
        save(&parent.join("sidebar.json"), true).unwrap();
        assert!(read_saved(&parent.join("sidebar.json")).unwrap().collapsed);
        fs::remove_dir_all(dir).unwrap();
    }
    #[test]
    fn only_current_main_controller_may_access_sidebar_preferences() {
        let origin = Url::parse("http://127.0.0.1:43210").unwrap();
        assert!(authorized("main", &origin, Some(&origin)));
        assert!(authorized(
            "main",
            &Url::parse("http://127.0.0.1:43210/#mastermind").unwrap(),
            Some(&origin)
        ));
        for (label, url) in [
            ("chrome", "http://127.0.0.1:43210/"),
            ("app-menu", "http://tauri.localhost/menu.html"),
            ("main", "http://127.0.0.1:43211/"),
            ("main", "about:srcdoc"),
            ("main", "http://127.0.0.1:43210/auth"),
            ("main", "http://127.0.0.1:43210/?redirect=x"),
            ("main", "tauri://localhost/index.html"),
            ("other", "http://127.0.0.1:43210/"),
        ] {
            assert!(
                !authorized(label, &Url::parse(url).unwrap(), Some(&origin)),
                "{label} {url}"
            );
        }
        assert!(!authorized("main", &origin, None));
    }
}
