fn main() {
    tauri_build::try_build(
        tauri_build::Attributes::new()
            .app_manifest(tauri_build::AppManifest::new().commands(&["frame_action"])),
    )
    .expect("build desktop assets and restricted frame permissions");
}
