use std::{
    ffi::OsString,
    path::PathBuf,
    process::{Command, Stdio},
    time::{SystemTime, UNIX_EPOCH},
};
use tauri::{Manager, Webview};
use url::Url;

pub fn route(host_id: &str, session_id: &str) -> Result<String, String> {
    if host_id.is_empty()
        || host_id.len() > 128
        || !host_id
            .bytes()
            .all(|c| c.is_ascii_alphanumeric() || c == b'-' || c == b'_')
        || session_id.len() != 32
        || !session_id
            .bytes()
            .all(|c| c.is_ascii_digit() || (b'a'..=b'f').contains(&c))
    {
        return Err("Choose an existing Relay terminal session".into());
    }
    Ok(format!("#host/{host_id}/session/{session_id}/window"))
}

pub fn arguments(args: Vec<OsString>) -> Result<Option<String>, String> {
    if args.is_empty() {
        return Ok(None);
    }
    if args.len() != 3 || args[0] != "--session-window" {
        return Err("Unsupported Relay desktop arguments".into());
    }
    let host = args[1].to_str().ok_or("Invalid machine identity")?;
    let session = args[2].to_str().ok_or("Invalid session identity")?;
    route(host, session).map(Some)
}

pub fn authorized(label: &str, url: &Url, origin: Option<&Url>) -> bool {
    label == "main"
        && url.path() == "/"
        && url.query().is_none()
        && url.username().is_empty()
        && url.password().is_none()
        && url.scheme() == "http"
        && url.host_str() == Some("127.0.0.1")
        && origin.is_some_and(|origin| crate::launch::same_origin(origin, url))
}

// Each process gets its own browser profile; cookies come only from the normal
// private controller bootstrap. It never borrows another window's login URL.
pub struct Profile(pub Option<PathBuf>);
impl Profile {
    pub fn new(app: &tauri::AppHandle, detached: bool) -> Result<Self, Box<dyn std::error::Error>> {
        if !detached {
            return Ok(Self(None));
        }
        let root = match std::env::var_os("RELAY_DESKTOP_STATE_DIR") {
            Some(value) => {
                let path = PathBuf::from(value);
                if !path.is_absolute() {
                    return Err("RELAY_DESKTOP_STATE_DIR must be absolute".into());
                }
                path.join("desktop").join("viewer-profiles")
            }
            None => app.path().app_cache_dir()?.join("viewer-profiles"),
        };
        std::fs::create_dir_all(&root)?;
        let info = std::fs::symlink_metadata(&root)?;
        let linked = info.file_type().is_symlink();
        #[cfg(windows)]
        let linked = {
            use std::os::windows::fs::MetadataExt;
            linked || info.file_attributes() & 0x400 != 0
        };
        if !info.is_dir() || linked {
            return Err("Viewer profile directory must not be a link".into());
        }
        let nonce = SystemTime::now().duration_since(UNIX_EPOCH)?.as_nanos();
        let path = root.join(format!("{}-{nonce}", std::process::id()));
        std::fs::create_dir(&path)?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o700))?;
        }
        Ok(Self(Some(path)))
    }
}
impl Drop for Profile {
    fn drop(&mut self) {
        if let Some(path) = &self.0 {
            let _ = std::fs::remove_dir_all(path);
        }
    }
}

pub fn page_loaded(view: &Webview, url: &Url) {
    let state = view.app_handle().state::<crate::DesktopState>();
    let Ok(origin) = state.origin.lock() else {
        return;
    };
    if !authorized(view.label(), url, origin.as_ref()) {
        return;
    }
    let Ok(pending) = state.session_route.lock() else {
        return;
    };
    if let Some(route) = pending.as_ref() {
        let route_json = serde_json::json!(route);
        let expected = serde_json::json!(url.as_str());
        // Preserve the assigned session after Reload or a private reconnect.
        let _ = view.eval(format!("if(location.href === {expected} && location.hash !== {route_json}) location.hash = {route_json};"));
    }
}

// Immutable process-role metadata. A detached viewer cannot become the primary
// request consumer by changing its hash; no additional IPC permission is granted.
pub fn primary_window_initialization(detached: bool) -> &'static str {
    if detached {
        "if(window===window.top)Object.defineProperty(window,'__RELAY_PRIMARY_WINDOW__',{value:false,writable:false,configurable:false});"
    } else {
        "if(window===window.top)Object.defineProperty(window,'__RELAY_PRIMARY_WINDOW__',{value:true,writable:false,configurable:false});"
    }
}

#[tauri::command]
pub async fn open_session_window(
    webview: Webview,
    host_id: String,
    session_id: String,
) -> Result<(), String> {
    route(&host_id, &session_id)?;
    let state = webview.app_handle().state::<crate::DesktopState>();
    let url = webview
        .url()
        .map_err(|_| "Could not verify this Relay window")?;
    let origin = state
        .origin
        .lock()
        .map_err(|_| "Could not verify this Relay window")?;
    if !authorized(webview.label(), &url, origin.as_ref()) {
        return Err(
            "Terminal windows can only be opened from the current Relay application".into(),
        );
    }
    drop(origin);
    let executable =
        std::env::current_exe().map_err(|_| "Could not locate the Relay desktop executable")?;
    let mut command = Command::new(executable);
    // WebView2's environment override otherwise wins over data_directory and
    // would make an independently launched viewer borrow the parent's profile.
    command.env_remove("WEBVIEW2_USER_DATA_FOLDER");
    command
        .args(["--session-window", &host_id, &session_id])
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null());
    #[cfg(windows)]
    {
        use std::os::windows::process::CommandExt;
        command.creation_flags(0x0800_0000);
    }
    let mut child = command
        .spawn()
        .map_err(|_| "Could not open another Relay window")?;
    // Reap only our own desktop child. Closing the parent leaves the child and
    // its runtime alive; no process-group or terminal-session termination.
    std::thread::spawn(move || {
        let _ = child.wait();
    });
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn primary_role_is_immutable_and_detached_cannot_consume_requests() {
        assert!(primary_window_initialization(false).contains("value:true"));
        assert!(primary_window_initialization(true).contains("value:false"));
        for detached in [false, true] {
            assert!(primary_window_initialization(detached).contains("window===window.top"));
            assert!(primary_window_initialization(detached)
                .contains("writable:false,configurable:false"));
        }
    }
    #[test]
    fn terminal_arguments_accept_only_literal_identities() {
        let id = "0123456789abcdef0123456789abcdef";
        assert_eq!(
            route("local", id).unwrap(),
            format!("#host/local/session/{id}/window")
        );
        assert_eq!(arguments(vec![]).unwrap(), None);
        assert_eq!(
            arguments(
                ["--session-window", "a-1_b", id]
                    .into_iter()
                    .map(Into::into)
                    .collect()
            )
            .unwrap(),
            Some(format!("#host/a-1_b/session/{id}/window"))
        );
        for host in [
            "",
            "../x",
            "a/b",
            "https://evil",
            "a#b",
            "a%20b",
            "a\nb",
            "$(run)",
        ] {
            assert!(route(host, id).is_err());
        }
        for session in [
            "",
            "ABCDEF0123456789abcdef0123456789",
            "../other",
            "0123456789abcdef0123456789abcdeg",
        ] {
            assert!(route("local", session).is_err());
        }
        assert!(arguments(
            ["--session-window", "local", id, "extra"]
                .into_iter()
                .map(Into::into)
                .collect()
        )
        .is_err());
    }
    #[test]
    fn only_the_authenticated_current_controller_page_can_spawn() {
        let origin = Url::parse("http://127.0.0.1:43210").unwrap();
        assert!(authorized("main", &origin, Some(&origin)));
        assert!(authorized(
            "main",
            &Url::parse("http://127.0.0.1:43210/#mastermind").unwrap(),
            Some(&origin)
        ));
        for (label, url) in [
            ("chrome", "http://127.0.0.1:43210/"),
            ("main", "http://127.0.0.1:43211/"),
            ("main", "http://localhost:43210/"),
            ("main", "http://user@127.0.0.1:43210/"),
            ("main", "http://127.0.0.1:43210/auth"),
            ("main", "http://127.0.0.1:43210/?url=evil"),
            ("main", "tauri://localhost/index.html"),
        ] {
            assert!(
                !authorized(label, &Url::parse(url).unwrap(), Some(&origin)),
                "{url}"
            );
        }
        assert!(!authorized("main", &origin, None));
    }
}
