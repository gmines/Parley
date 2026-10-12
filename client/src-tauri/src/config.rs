//! Non-secret settings persisted as JSON in the app's config directory.

use std::path::PathBuf;
use std::sync::Mutex;

use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Config {
    /// Last server the user logged in to.
    pub server_url: Option<String>,
}

pub struct ConfigStore {
    path: PathBuf,
    lock: Mutex<()>,
}

impl ConfigStore {
    pub fn new(path: PathBuf) -> Self {
        Self { path, lock: Mutex::new(()) }
    }

    pub fn load(&self) -> Config {
        let _g = self.lock.lock().unwrap();
        std::fs::read(&self.path)
            .ok()
            .and_then(|b| serde_json::from_slice(&b).ok())
            .unwrap_or_default()
    }

    pub fn update(&self, f: impl FnOnce(&mut Config)) {
        let _g = self.lock.lock().unwrap();
        let mut cfg: Config = std::fs::read(&self.path)
            .ok()
            .and_then(|b| serde_json::from_slice(&b).ok())
            .unwrap_or_default();
        f(&mut cfg);
        let result = self
            .path
            .parent()
            .map_or(Ok(()), std::fs::create_dir_all)
            .and_then(|_| {
                // Write-then-rename so a crash can't leave a truncated file.
                let tmp = self.path.with_extension("json.tmp");
                std::fs::write(&tmp, serde_json::to_vec_pretty(&cfg).unwrap())?;
                std::fs::rename(&tmp, &self.path)
            });
        if let Err(e) = result {
            log::warn!("could not save config to {}: {e}", self.path.display());
        }
    }
}
