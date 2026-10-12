use crate::launch::LaunchError;
use std::{os::windows::process::CommandExt, path::Path, process::Command, time::Instant};

const CREATE_NO_WINDOW: u32 = 0x0800_0000;
const CONTROLLER: &str = "relay-controller-windows-amd64.exe";

pub fn show_startup_failure() {
    // A GUI executable has no console, and a missing WebView2 Runtime prevents
    // even the bundled error page from opening. Show fixed native guidance;
    // never put helper diagnostics or authentication URLs into this dialog.
    #[link(name = "user32")]
    extern "system" {
        fn MessageBoxW(
            window: *mut std::ffi::c_void,
            message: *const u16,
            caption: *const u16,
            kind: u32,
        ) -> i32;
    }
    let message: Vec<u16> = "Relay could not open its window.\n\nExtract the complete app folder before launching Relay and make sure Microsoft Edge WebView2 Runtime is installed. See README.txt in the portable folder for Microsoft's download link.\n\nNo changes were made to your Windows settings."
        .encode_utf16().chain(Some(0)).collect();
    let caption: Vec<u16> = "Relay could not start"
        .encode_utf16()
        .chain(Some(0))
        .collect();
    // MessageBoxW reads the two live, terminated UTF-16 buffers synchronously.
    unsafe {
        MessageBoxW(
            std::ptr::null_mut(),
            message.as_ptr(),
            caption.as_ptr(),
            0x10,
        );
    }
}

pub fn configure_frame(_window: &tauri::Window) -> tauri::Result<()> {
    Ok(())
}

pub fn validate_bundle(bundle: &Path) -> Result<(), LaunchError> {
    if std::env::consts::ARCH != "x86_64" {
        return Err(LaunchError::new(
            "This Relay desktop package requires Windows x64.",
        ));
    }
    if !bundle.join(CONTROLLER).is_file() {
        return Err(LaunchError::new(
            "Relay's bundled Windows controller is missing. Extract the complete Relay package before opening the app.",
        ));
    }
    Ok(())
}

pub fn controller_command(bundle: &Path, _deadline: Instant) -> Result<Command, LaunchError> {
    let mut command = Command::new(bundle.join(CONTROLLER));
    command.creation_flags(CREATE_NO_WINDOW);
    command.arg("desktop").arg("--binaries").arg(bundle);
    Ok(command)
}

pub fn default_local() -> bool {
    false
}

pub fn shell_error_url() -> &'static str {
    "http://tauri.localhost/error.html"
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::ffi::OsStr;

    #[test]
    fn controller_path_and_bundle_remain_literal_arguments() {
        let bundle = Path::new(r"C:\Users\a b\Relay & $(untouched)\runtime");
        let command = controller_command(bundle, Instant::now()).expect("native command");
        assert_eq!(command.get_program(), bundle.join(CONTROLLER).as_os_str());
        let args: Vec<_> = command.get_args().collect();
        assert_eq!(
            args,
            [
                OsStr::new("desktop"),
                OsStr::new("--binaries"),
                bundle.as_os_str()
            ]
        );
    }

    #[test]
    fn native_windows_starts_with_remote_hosts_only() {
        assert!(!default_local());
        assert_eq!(shell_error_url(), "http://tauri.localhost/error.html");
    }
}
