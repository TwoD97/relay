#[cfg(target_os = "linux")]
mod linux;

#[cfg(target_os = "linux")]
pub use linux::{
    configure_frame, controller_command, default_local, shell_error_url, validate_bundle,
};

#[cfg(target_os = "windows")]
mod windows;

#[cfg(target_os = "windows")]
pub use windows::{
    configure_frame, controller_command, default_local, shell_error_url, validate_bundle,
};

#[cfg(not(any(target_os = "linux", target_os = "windows")))]
compile_error!("Relay desktop currently supports Linux and Windows.");
