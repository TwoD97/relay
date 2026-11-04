//! Client layout references only. No runtime mutation, terminal input or credentials.
use serde::{Deserialize, Serialize};
use std::{
    fs::{self, OpenOptions},
    io::{Read, Write},
    path::{Path, PathBuf},
    sync::atomic::{AtomicU64, Ordering},
};
use tauri::{Manager, Webview};

const MAX_FILE: u64 = 128 * 1024;
static NEXT_TEMP: AtomicU64 = AtomicU64::new(0);
#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(deny_unknown_fields, rename_all = "camelCase")]
pub struct Pane {
    host: String,
    session: String,
    created_at: String,
}
#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(deny_unknown_fields, rename_all = "camelCase")]
pub struct Group {
    id: String,
    title: String,
    host_name: String,
    workspace: String,
    panes: Vec<Pane>,
    collapsed: bool,
}
#[derive(Clone, Debug, Default, Deserialize, Serialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
pub struct Snapshot {
    revision: u64,
    groups: Vec<Group>,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Change {
    revision: u64,
    group: Option<Group>,
    remove: Option<String>,
}
#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct Saved {
    format: u8,
    snapshot: Snapshot,
}
fn id(value: &str) -> bool {
    value.len() == 36
        && value
            .bytes()
            .all(|c| c.is_ascii_hexdigit() && !c.is_ascii_uppercase() || c == b'-')
}
fn field(value: &str) -> bool {
    !value.is_empty() && value.chars().count() <= 256 && !value.chars().any(char::is_control)
}
fn valid(group: &Group) -> bool {
    id(&group.id)
        && field(&group.title)
        && field(&group.host_name)
        && field(&group.workspace)
        && !group.panes.is_empty()
        && group.panes.len() <= 4
        && group.panes.iter().enumerate().all(|(i, p)| {
            field(&p.host)
                && field(&p.session)
                && field(&p.created_at)
                && !group.panes[..i]
                    .iter()
                    .any(|other| other.host == p.host && other.session == p.session)
        })
}
fn valid_snapshot(s: &Snapshot) -> bool {
    s.revision < 9_007_199_254_740_991
        && s.groups.len() <= 32
        && s.groups
            .iter()
            .enumerate()
            .all(|(i, g)| valid(g) && !s.groups[..i].iter().any(|other| other.id == g.id))
}
fn linked(meta: &fs::Metadata) -> bool {
    #[cfg(windows)]
    {
        use std::os::windows::fs::MetadataExt;
        meta.file_attributes() & 0x400 != 0
    }
    #[cfg(not(windows))]
    {
        meta.file_type().is_symlink()
    }
}
fn options() -> OpenOptions {
    let mut options = OpenOptions::new();
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.mode(0o600);
    }
    #[cfg(target_os = "linux")]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.custom_flags(0x20000);
    }
    #[cfg(windows)]
    {
        use std::os::windows::fs::OpenOptionsExt;
        options.custom_flags(0x0020_0000);
    }
    options
}
fn regular(path: &Path) -> Result<(), String> {
    match fs::symlink_metadata(path) {
        Ok(info) if !info.is_file() || linked(&info) => {
            Err("Terminal groups must use a regular private file".into())
        }
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(_) => Err("Could not inspect saved terminal groups".into()),
        _ => Ok(()),
    }
}
fn read(path: &Path) -> Result<Snapshot, String> {
    regular(path)?;
    let file = match options().read(true).open(path) {
        Ok(file) => file,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(Snapshot::default()),
        Err(_) => return Err("Could not read saved terminal groups".into()),
    };
    let info = file
        .metadata()
        .map_err(|_| "Could not inspect saved terminal groups")?;
    if !info.is_file() || linked(&info) || info.len() > MAX_FILE {
        return Err("Saved terminal groups are not a bounded regular file".into());
    }
    let mut bytes = Vec::new();
    file.take(MAX_FILE + 1)
        .read_to_end(&mut bytes)
        .map_err(|_| "Could not read saved terminal groups")?;
    if bytes.len() as u64 > MAX_FILE {
        return Err("Saved terminal groups are too large".into());
    }
    let saved: Saved = serde_json::from_slice(&bytes)
        .map_err(|_| "Saved terminal groups are invalid; the existing file was preserved")?;
    if saved.format != 1 || !valid_snapshot(&saved.snapshot) {
        return Err(
            "Saved terminal groups have an unsupported format or invalid identities".into(),
        );
    }
    Ok(saved.snapshot)
}
fn transact(path: &Path, change: Option<Change>) -> Result<Snapshot, String> {
    let parent = path
        .parent()
        .ok_or("Terminal group directory is unavailable")?;
    let mut builder = fs::DirBuilder::new();
    builder.recursive(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::DirBuilderExt;
        builder.mode(0o700);
    }
    builder
        .create(parent)
        .map_err(|_| "Could not create the terminal group directory")?;
    // A stable file lock serializes separate native windows/processes. Never
    // unlink it: replacing a lock inode would split the serialization boundary.
    let lock_path = parent.join("terminal-groups.lock");
    regular(&lock_path)?;
    let lock = options()
        .read(true)
        .write(true)
        .create(true)
        .truncate(false)
        .open(&lock_path)
        .map_err(|_| "Could not open the terminal group lock")?;
    let info = lock
        .metadata()
        .map_err(|_| "Could not inspect the terminal group lock")?;
    if !info.is_file() || linked(&info) {
        return Err("Terminal group lock must be a regular file".into());
    }
    lock.try_lock().map_err(|_| {
        "Terminal groups are busy in another window. Reload saved groups before editing."
    })?;
    let mut value = read(path)?;
    let Some(change) = change else {
        return Ok(value);
    };
    if value.revision != change.revision {
        return Err(
            "Terminal groups changed in another window. Reload saved groups before editing.".into(),
        );
    }
    match (change.group, change.remove) {
        (Some(group), None) if valid(&group) => {
            if let Some(existing) = value.groups.iter_mut().find(|g| g.id == group.id) {
                *existing = group;
            } else {
                value.groups.push(group);
            }
        }
        (None, Some(id_value)) if id(&id_value) => value.groups.retain(|g| g.id != id_value),
        _ => return Err("Invalid terminal group change".into()),
    }
    value.revision += 1;
    if !valid_snapshot(&value) {
        return Err("You can save up to 32 terminal groups with four panes each".into());
    }
    let bytes = serde_json::to_vec(&Saved {
        format: 1,
        snapshot: value.clone(),
    })
    .map_err(|_| "Could not encode terminal groups")?;
    if bytes.len() as u64 > MAX_FILE {
        return Err("Saved terminal groups are too large".into());
    }
    regular(path)?;
    let temp = parent.join(format!(
        ".terminal-groups-{}-{}.tmp",
        std::process::id(),
        NEXT_TEMP.fetch_add(1, Ordering::Relaxed)
    ));
    let mut file = options()
        .write(true)
        .create_new(true)
        .open(&temp)
        .map_err(|_| "Could not create a private terminal group file")?;
    let result = (|| -> std::io::Result<()> {
        file.write_all(&bytes)?;
        file.sync_all()?;
        drop(file);
        fs::rename(&temp, path)?;
        #[cfg(unix)]
        fs::File::open(parent)?.sync_all()?;
        Ok(())
    })();
    let _ = fs::remove_file(temp);
    result.map_err(|_| {
        "Saving terminal groups was not confirmed. Reload saved groups before editing."
    })?;
    Ok(value)
}
fn store_path(app: &tauri::AppHandle) -> Result<PathBuf, String> {
    match std::env::var_os("RELAY_DESKTOP_STATE_DIR") {
        Some(value) => {
            let path = PathBuf::from(value);
            if !path.is_absolute() {
                return Err("RELAY_DESKTOP_STATE_DIR must be absolute".into());
            }
            Ok(path.join("desktop").join("terminal-groups.json"))
        }
        None => app
            .path()
            .app_config_dir()
            .map(|p| p.join("terminal-groups.json"))
            .map_err(|_| "Terminal group directory is unavailable".into()),
    }
}
#[tauri::command]
pub async fn terminal_groups(webview: Webview, change: Option<Change>) -> Result<Snapshot, String> {
    let url = webview
        .url()
        .map_err(|_| "Terminal group origin is unavailable")?;
    let state = webview.app_handle().state::<crate::DesktopState>();
    let origin = state
        .origin
        .lock()
        .map_err(|_| "Terminal group origin is unavailable")?;
    if !crate::ui_preferences::authorized(webview.label(), &url, origin.as_ref()) {
        return Err("Terminal groups are only available in the current Relay application".into());
    }
    drop(origin);
    transact(&store_path(webview.app_handle())?, change)
}

#[cfg(test)]
mod tests {
    use super::*;
    fn temp() -> PathBuf {
        let p = std::env::temp_dir().join(format!(
            "relay-groups-test-{}-{}",
            std::process::id(),
            NEXT_TEMP.fetch_add(1, Ordering::Relaxed)
        ));
        fs::create_dir(&p).unwrap();
        p
    }
    fn group(n: usize) -> Group {
        Group {
            id: format!("00000000-0000-0000-0000-{n:012x}"),
            title: "Original terminal".into(),
            host_name: "Fixture host".into(),
            workspace: "Workspace".into(),
            panes: vec![Pane {
                host: "host".into(),
                session: "session".into(),
                created_at: "2026-11-04T10:00:00Z".into(),
            }],
            collapsed: false,
        }
    }
    fn put(revision: u64, group: Group) -> Option<Change> {
        Some(Change {
            revision,
            group: Some(group),
            remove: None,
        })
    }
    #[test]
    fn persists_exact_identities_and_compare_and_swap_across_independent_windows() {
        let dir = temp();
        let path = dir.join("terminal-groups.json");
        assert_eq!(transact(&path, None).unwrap().revision, 0);
        let saved = transact(&path, put(0, group(1))).unwrap();
        assert_eq!(saved.revision, 1);
        assert_eq!(transact(&path, None).unwrap(), saved);
        assert!(transact(&path, put(0, group(2))).is_err());
        assert_eq!(read(&path).unwrap(), saved);
        assert_eq!(transact(&path, put(1, group(2))).unwrap().groups.len(), 2);
        let removed = transact(
            &path,
            Some(Change {
                revision: 2,
                group: None,
                remove: Some(group(1).id),
            }),
        )
        .unwrap();
        assert_eq!(removed.groups, vec![group(2)]);
        fs::remove_dir_all(dir).unwrap();
    }
    #[test]
    fn rejects_reused_identities_invalid_bounds_and_preserves_corrupt_file() {
        let dir = temp();
        let path = dir.join("terminal-groups.json");
        let mut g = group(1);
        g.panes.push(g.panes[0].clone());
        g.panes[1].created_at = "replacement".into();
        assert!(transact(&path, put(0, g)).is_err());
        for n in 0..32 {
            transact(&path, put(n as u64, group(n))).unwrap();
        }
        assert!(transact(&path, put(32, group(33))).is_err());
        assert_eq!(read(&path).unwrap().revision, 32);
        fs::write(&path, b"invalid").unwrap();
        assert!(transact(&path, None).is_err());
        assert!(transact(&path, put(0, group(1))).is_err());
        assert_eq!(fs::read(&path).unwrap(), b"invalid");
        fs::remove_dir_all(dir).unwrap();
    }
    #[test]
    fn concurrent_writers_never_overwrite_a_committed_revision() {
        let dir = temp();
        let path = dir.join("terminal-groups.json");
        transact(&path, None).unwrap();
        let barrier = std::sync::Arc::new(std::sync::Barrier::new(8));
        let joins: Vec<_> = (0..8)
            .map(|n| {
                let path = path.clone();
                let barrier = barrier.clone();
                std::thread::spawn(move || {
                    barrier.wait();
                    transact(&path, put(0, group(n))).is_ok()
                })
            })
            .collect();
        let successes = joins
            .into_iter()
            .map(|j| usize::from(j.join().unwrap()))
            .sum::<usize>();
        assert_eq!(successes, 1);
        assert_eq!(read(&path).unwrap().groups.len(), 1);
        fs::remove_dir_all(dir).unwrap();
    }
    #[cfg(unix)]
    #[test]
    fn symlinked_data_and_lock_targets_are_never_followed() {
        use std::os::unix::fs::symlink;
        let dir = temp();
        let path = dir.join("terminal-groups.json");
        let target = dir.join("target");
        fs::write(&target, b"private").unwrap();
        symlink(&target, &path).unwrap();
        assert!(transact(&path, None).is_err());
        assert!(transact(&path, put(0, group(1))).is_err());
        fs::remove_file(&path).unwrap();
        fs::remove_file(dir.join("terminal-groups.lock")).unwrap();
        symlink(&target, dir.join("terminal-groups.lock")).unwrap();
        assert!(transact(&path, put(0, group(1))).is_err());
        assert_eq!(fs::read(target).unwrap(), b"private");
        fs::remove_dir_all(dir).unwrap();
    }
}
