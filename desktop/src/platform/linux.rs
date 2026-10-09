use crate::launch::LaunchError;
use std::{path::Path, process::Command, time::Instant};

pub fn configure_frame(window: &tauri::Window) -> tauri::Result<()> {
    use gtk::prelude::*;
    use tauri::Manager;
    // Wry's GTK child views are packed into a Box, which ignores set_bounds.
    // A native Overlay gives the content its own area and keeps frame/menu
    // hit testing and sizing independent of the controller document.
    for label in ["main", "chrome", "app-menu"] {
        let window = window.clone();
        if let Some(view) = window.app_handle().get_webview(label) {
            view.with_webview(move |native| {
                let Ok(root) = window.default_vbox() else {
                    return;
                };
                let overlay = root
                    .children()
                    .into_iter()
                    .find_map(|child| child.downcast::<gtk::Overlay>().ok())
                    .unwrap_or_else(|| {
                        let overlay = gtk::Overlay::new();
                        root.pack_start(&overlay, true, true, 0);
                        overlay.show();
                        overlay
                    });
                let widget = native.inner();
                if let Some(parent) = widget
                    .parent()
                    .and_then(|parent| parent.downcast::<gtk::Container>().ok())
                {
                    parent.remove(&widget);
                }
                widget.set_hexpand(true);
                match label {
                    "main" => {
                        widget.set_margin_top(crate::frame::HEIGHT as i32);
                        widget.set_vexpand(true);
                        overlay.add(&widget);
                    }
                    "chrome" => {
                        widget.set_valign(gtk::Align::Start);
                        widget.set_height_request(crate::frame::HEIGHT as i32);
                        overlay.add_overlay(&widget);
                    }
                    _ => {
                        widget.set_halign(gtk::Align::Start);
                        widget.set_valign(gtk::Align::Start);
                        widget.set_margin_start(8);
                        widget.set_margin_top(crate::frame::HEIGHT as i32 + 4);
                        widget.set_size_request(
                            crate::frame::MENU_WIDTH as i32,
                            crate::frame::MENU_HEIGHT as i32,
                        );
                        overlay.add_overlay(&widget);
                    }
                }
            })?;
        }
    }
    Ok(())
}

fn controller_name() -> Result<&'static str, LaunchError> {
    match std::env::consts::ARCH {
        "x86_64" => Ok("relay-linux-amd64"),
        "aarch64" => Ok("relay-linux-arm64"),
        _ => Err(LaunchError::new(
            "Relay desktop supports Linux amd64 and arm64.",
        )),
    }
}

pub fn validate_bundle(bundle: &Path) -> Result<(), LaunchError> {
    let binary = bundle.join(controller_name()?);
    if !binary.is_file() {
        return Err(LaunchError::new(format!(
            "The bundled Relay controller is missing at {}. Reinstall Relay or build its desktop resources.",
            binary.display()
        )));
    }
    Ok(())
}

pub fn controller_command(bundle: &Path, _deadline: Instant) -> Result<Command, LaunchError> {
    let mut command = Command::new(bundle.join(controller_name()?));
    command.arg("desktop").arg("--binaries").arg(bundle);
    Ok(command)
}

pub fn shell_error_url() -> &'static str {
    "tauri://localhost/error.html"
}

pub fn default_local() -> bool {
    true
}
