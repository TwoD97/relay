use serde::Deserialize;
use std::{
    ffi::OsString,
    fmt,
    io::Read,
    path::{Path, PathBuf},
    process::{Command, Stdio},
    sync::mpsc,
    thread,
    time::{Duration, Instant},
};
use url::Url;

const MAX_OUTPUT: u64 = 64 * 1024;
const LAUNCH_TIMEOUT: Duration = Duration::from_secs(30);

#[derive(Debug)]
pub struct LaunchError(String);

impl LaunchError {
    pub fn new(message: impl Into<String>) -> Self {
        Self(message.into())
    }
}

impl fmt::Display for LaunchError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.0)
    }
}

impl std::error::Error for LaunchError {}

#[derive(Clone)]
pub struct Launcher {
    bundle: PathBuf,
    state_dir: Option<OsString>,
    runtime_dir: Option<OsString>,
    local: bool,
}

// Deliberately no Debug: this value contains a one-time authentication secret.
pub struct Launch {
    pub url: Url,
    pub address: Url,
}

#[derive(Deserialize)]
struct LaunchResponse {
    url: String,
    address: String,
    version: String,
    protocol: u32,
    pid: u32,
}

impl Launcher {
    pub fn from_environment(resources: &Path) -> Result<Self, LaunchError> {
        let bundle = match std::env::var_os("RELAY_DESKTOP_BUNDLE") {
            Some(path) => {
                let path = PathBuf::from(path);
                if !path.is_absolute() {
                    return Err(LaunchError::new(
                        "RELAY_DESKTOP_BUNDLE must be an absolute directory.",
                    ));
                }
                path
            }
            None => resources.join("runtime"),
        };
        crate::platform::validate_bundle(&bundle)?;
        let local = match std::env::var("RELAY_DESKTOP_LOCAL").as_deref() {
            Ok("0") => false,
            Ok("1") => true,
            Err(std::env::VarError::NotPresent) => crate::platform::default_local(),
            _ => return Err(LaunchError::new("RELAY_DESKTOP_LOCAL must be 0 or 1.")),
        };
        Ok(Self {
            bundle,
            state_dir: std::env::var_os("RELAY_DESKTOP_STATE_DIR"),
            runtime_dir: std::env::var_os("RELAY_DESKTOP_RUNTIME_DIR"),
            local,
        })
    }

    pub fn launch(&self) -> Result<Launch, LaunchError> {
        let deadline = Instant::now() + LAUNCH_TIMEOUT;
        let mut command = crate::platform::controller_command(&self.bundle, deadline)?;
        if let Some(path) = &self.state_dir {
            command.arg("--state-dir").arg(path);
        }
        if let Some(path) = &self.runtime_dir {
            command.arg("--runtime-dir").arg(path);
        }
        command.arg(if self.local {
            "--local=true"
        } else {
            "--local=false"
        });
        let bytes = run_helper(command, deadline.saturating_duration_since(Instant::now()))?;
        parse_launch(&bytes)
    }
}

fn run_helper(mut command: Command, timeout: Duration) -> Result<Vec<u8>, LaunchError> {
    let mut child = command
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        // GUI Windows builds have no console. Capture bounded diagnostics for
        // the native error page without ever including the JSON/auth stdout.
        .stderr(Stdio::piped())
        .spawn()
        .map_err(|error| {
            LaunchError::new(format!("Could not start the bundled controller: {error}"))
        })?;
    let stdout = match child.stdout.take() {
        Some(stdout) => stdout,
        None => {
            let _ = child.kill();
            let _ = child.wait();
            return Err(LaunchError::new(
                "Could not read the controller launch response.",
            ));
        }
    };
    let stderr = match child.stderr.take() {
        Some(stderr) => stderr,
        None => {
            let _ = child.kill();
            let _ = child.wait();
            return Err(LaunchError::new(
                "Could not read the controller diagnostic.",
            ));
        }
    };
    // Read concurrently so a malformed helper cannot fill its pipe and prevent
    // the timeout. The byte cap also bounds retained authentication data.
    let (sender, receiver) = mpsc::sync_channel(1);
    thread::spawn(move || {
        let mut bytes = Vec::new();
        let result = stdout
            .take(MAX_OUTPUT + 1)
            .read_to_end(&mut bytes)
            .map(|_| bytes);
        let _ = sender.send(result);
    });
    let (error_sender, error_receiver) = mpsc::sync_channel(1);
    thread::spawn(move || {
        let mut bytes = Vec::new();
        let result = stderr
            .take(MAX_OUTPUT + 1)
            .read_to_end(&mut bytes)
            .map(|_| bytes);
        let _ = error_sender.send(result);
    });
    let deadline = Instant::now() + timeout;
    let status = loop {
        match child.try_wait() {
            Ok(Some(status)) => break status,
            Ok(None) if Instant::now() < deadline => thread::sleep(Duration::from_millis(25)),
            Ok(None) => {
                let _ = child.kill();
                let _ = child.wait();
                return Err(LaunchError::new("Relay controller startup timed out after 30 seconds. Choose Reconnect to try again."));
            }
            Err(error) => {
                let _ = child.kill();
                let _ = child.wait();
                return Err(LaunchError::new(format!(
                    "Could not wait for the Relay controller: {error}"
                )));
            }
        }
    };
    if !status.success() {
        let diagnostic = error_receiver
            .recv_timeout(
                deadline
                    .saturating_duration_since(Instant::now())
                    .min(Duration::from_millis(250)),
            )
            .ok()
            .and_then(Result::ok)
            .map(|bytes| safe_diagnostic(&bytes))
            .unwrap_or_default();
        return Err(LaunchError::new(format!(
            "Relay controller launch failed ({status}). {}",
            if diagnostic.is_empty() {
                "Choose Reconnect to try again."
            } else {
                &diagnostic
            }
        )));
    }
    let bytes = receiver
        .recv_timeout(deadline.saturating_duration_since(Instant::now()))
        .map_err(|_| LaunchError::new("Could not read the controller launch response."))?
        .map_err(|_| LaunchError::new("Could not read the controller launch response."))?;
    if bytes.len() as u64 > MAX_OUTPUT {
        return Err(LaunchError::new(
            "The controller launch response exceeded 64 KiB.",
        ));
    }
    Ok(bytes)
}

fn safe_diagnostic(bytes: &[u8]) -> String {
    let text = String::from_utf8_lossy(bytes);
    let text: String = text
        .chars()
        .filter(|c| !c.is_control() || matches!(c, '\n' | '\t'))
        .take(4096)
        .collect();
    let mut remaining = text.as_str();
    let mut sanitized = String::new();
    while let Some(start) = remaining.to_ascii_lowercase().find("token=") {
        sanitized.push_str(&remaining[..start]);
        sanitized.push_str("token=[redacted]");
        let value = &remaining[start + 6..];
        let end = value
            .char_indices()
            .find_map(|(offset, c)| {
                (c.is_whitespace() || matches!(c, '&' | '#' | '\'' | '"' | '<' | '>'))
                    .then_some(offset)
            })
            .unwrap_or(value.len());
        remaining = &value[end..];
    }
    sanitized.push_str(remaining);
    sanitized.trim().to_owned()
}

pub fn parse_launch(bytes: &[u8]) -> Result<Launch, LaunchError> {
    if bytes.len() as u64 > MAX_OUTPUT {
        return Err(LaunchError::new(
            "The controller launch response exceeded 64 KiB.",
        ));
    }
    // Parse errors deliberately omit response content and URLs: they may contain
    // credentials, including when a malformed child emits arbitrary output.
    let response: LaunchResponse = serde_json::from_slice(bytes)
        .map_err(|_| LaunchError::new("The controller returned an invalid launch response."))?;
    if response.protocol != 1 || response.pid == 0 || response.version.is_empty() {
        return Err(LaunchError::new(
            "The controller launch response is incompatible with this desktop app.",
        ));
    }
    let address = Url::parse(&response.address)
        .map_err(|_| LaunchError::new("The controller returned an invalid local address."))?;
    let url = Url::parse(&response.url)
        .map_err(|_| LaunchError::new("The controller returned an invalid sign-in address."))?;
    if !response.address.starts_with("http://127.0.0.1:")
        || !response.url.starts_with("http://127.0.0.1:")
        || !is_loopback(&address)
        || address.path() != "/"
        || address.query().is_some()
        || address.fragment().is_some()
        || !same_origin(&address, &url)
        || url.path() != "/auth"
        || url.fragment().is_some()
    {
        return Err(LaunchError::new(
            "The controller did not return a trusted local sign-in address.",
        ));
    }
    let pairs: Vec<_> = url.query_pairs().collect();
    if pairs.len() != 1
        || pairs[0].0 != "token"
        || pairs[0].1.len() < 32
        || pairs[0].1.len() > 512
        || !pairs[0]
            .1
            .bytes()
            .all(|c| c.is_ascii_alphanumeric() || c == b'-' || c == b'_')
    {
        return Err(LaunchError::new(
            "The controller returned an invalid one-time sign-in token.",
        ));
    }
    Ok(Launch { url, address })
}

fn is_loopback(url: &Url) -> bool {
    url.scheme() == "http"
        && url.host_str() == Some("127.0.0.1")
        && url.port_or_known_default().is_some_and(|port| port > 0)
        && url.username().is_empty()
        && url.password().is_none()
}

pub fn same_origin(address: &Url, candidate: &Url) -> bool {
    is_loopback(candidate) && address.origin() == candidate.origin()
}

pub fn is_shell_page(url: &Url) -> bool {
    matches!(
        (url.scheme(), url.host_str()),
        ("tauri", Some("localhost")) | ("http", Some("tauri.localhost"))
    ) && matches!(url.path(), "/index.html" | "/error.html" | "/")
        && url.query().is_none()
        && url.fragment().is_none()
        && url.username().is_empty()
        && url.password().is_none()
        && url.port().is_none()
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn response(url: &str, address: &str) -> Vec<u8> {
        serde_json::to_vec(
            &json!({"url":url,"address":address,"version":"0.1.0","pid":123,"protocol":1}),
        )
        .expect("fixture")
    }

    const TOKEN: &str = "a0123456789abcdef0123456789abcdef";

    #[test]
    fn accepts_exact_loopback_launch_and_navigation() {
        let launch = parse_launch(&response(
            &format!("http://127.0.0.1:7340/auth?token={TOKEN}"),
            "http://127.0.0.1:7340",
        ))
        .expect("valid launch");
        assert!(same_origin(
            &launch.address,
            &Url::parse("http://127.0.0.1:7340/api/state").expect("url")
        ));
        assert!(!same_origin(
            &launch.address,
            &Url::parse("http://127.0.0.1:7341/").expect("url")
        ));
        assert!(!same_origin(
            &launch.address,
            &Url::parse("http://user@127.0.0.1:7340/").expect("url")
        ));
    }

    #[test]
    fn rejects_untrusted_launch_addresses_without_leaking_secrets() {
        for url in [
            format!("https://127.0.0.1:7340/auth?token={TOKEN}"),
            format!("http://localhost:7340/auth?token={TOKEN}"),
            format!("http://127.1:7340/auth?token={TOKEN}"),
            format!("http://2130706433:7340/auth?token={TOKEN}"),
            format!("http://127.0.0.1:7341/auth?token={TOKEN}"),
            format!("http://user@127.0.0.1:7340/auth?token={TOKEN}"),
            format!("http://127.0.0.1:7340/auth?token={TOKEN}#fragment"),
            format!("http://127.0.0.1:7340/auth?token={TOKEN}&token=duplicate"),
            format!("http://127.0.0.1:7340/?token={TOKEN}"),
            "javascript:alert(1)".to_owned(),
            "file:///etc/passwd".to_owned(),
        ] {
            let result = parse_launch(&response(&url, "http://127.0.0.1:7340"));
            let error = result.err().expect("must reject").to_string();
            assert!(!error.contains(TOKEN));
            assert!(!error.contains(&url));
        }
        for address in [
            "http://127.0.0.1:7340/path",
            "http://127.0.0.1:7340?x=y",
            "http://127.0.0.1:7340#fragment",
            "http://127.0.0.1:0",
        ] {
            assert!(
                parse_launch(&response(&format!("{address}/auth?token={TOKEN}"), address)).is_err()
            );
        }
    }

    #[test]
    fn bounds_and_validates_protocol_response() {
        assert!(parse_launch(&vec![b' '; MAX_OUTPUT as usize + 1]).is_err());
        assert!(parse_launch(b"not JSON").is_err());
        for patch in [
            json!({"protocol":2}),
            json!({"pid":0}),
            json!({"version":""}),
        ] {
            let mut value: serde_json::Value = serde_json::from_slice(&response(
                &format!("http://127.0.0.1:7340/auth?token={TOKEN}"),
                "http://127.0.0.1:7340",
            ))
            .expect("fixture");
            value
                .as_object_mut()
                .expect("object")
                .extend(patch.as_object().expect("patch").clone());
            assert!(parse_launch(&serde_json::to_vec(&value).expect("fixture")).is_err());
        }
    }

    #[test]
    fn only_bundled_shell_pages_are_local_navigation_targets() {
        for url in [
            "tauri://localhost/index.html",
            "tauri://localhost/error.html",
            "http://tauri.localhost/",
        ] {
            assert!(is_shell_page(&Url::parse(url).expect("url")));
        }
        for url in [
            "file:///index.html",
            "tauri://other/index.html",
            "tauri://localhost/api",
            "tauri://localhost/index.html?url=https://example.com",
            "tauri://localhost:7340/index.html",
        ] {
            assert!(!is_shell_page(&Url::parse(url).expect("url")));
        }
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn helper_timeout_terminates_and_reaps_only_the_helper() {
        let mut command = Command::new("sleep");
        command.arg("10");
        let started = Instant::now();
        assert!(run_helper(command, Duration::from_millis(75)).is_err());
        assert!(started.elapsed() < Duration::from_secs(2));
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn helper_output_is_bounded_and_failure_is_not_a_launch_response() {
        let mut oversized = Command::new("head");
        oversized.args(["-c", "65537", "/dev/zero"]);
        let error = run_helper(oversized, Duration::from_secs(2))
            .expect_err("oversized helper must fail")
            .to_string();
        assert!(error.contains("64 KiB"));

        let nonzero = Command::new("false");
        assert!(run_helper(nonzero, Duration::from_secs(2)).is_err());
    }

    #[test]
    fn diagnostics_are_bounded_strip_controls_and_redact_auth_urls() {
        let diagnostic = safe_diagnostic(
            b"\x1bError\0: http://127.0.0.1:7/auth?token=supersecret&next=ok\nTOKEN=othersecret\n",
        );
        assert!(!diagnostic.contains("supersecret"));
        assert!(!diagnostic.contains("othersecret"));
        assert!(!diagnostic.contains('\0'));
        assert!(!diagnostic.contains('\x1b'));
        assert!(diagnostic.contains("token=[redacted]&next=ok"));
        assert!(safe_diagnostic(&vec![b'x'; 8192]).len() <= 4096);
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn failed_helper_surfaces_stderr_but_never_authentication_stdout() {
        let mut command = Command::new("/bin/sh");
        command.args([
            "-c",
            "printf 'stdout-auth-secret'; printf 'Configure OpenSSH first\\n' >&2; exit 1",
        ]);
        let error = run_helper(command, Duration::from_secs(2))
            .expect_err("failed helper")
            .to_string();
        assert!(error.contains("Configure OpenSSH first"));
        assert!(!error.contains("stdout-auth-secret"));
    }
}
