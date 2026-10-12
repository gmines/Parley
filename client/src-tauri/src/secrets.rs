//! Refresh-token storage in the OS credential store: Secret Service on Linux,
//! Keychain on macOS, Credential Manager on Windows.
//!
//! Failures are logged, never fatal. Without a working credential store the
//! app still works, but you have to log in again on every launch.

const SERVICE: &str = "parley";

pub trait SecretStore: Send + Sync {
    fn get(&self, account: &str) -> Option<String>;
    fn set(&self, account: &str, secret: &str);
    fn delete(&self, account: &str);
}

pub struct Keyring;

impl SecretStore for Keyring {
    fn get(&self, account: &str) -> Option<String> {
        match keyring::Entry::new(SERVICE, account).and_then(|e| e.get_password()) {
            Ok(s) => Some(s),
            Err(keyring::Error::NoEntry) => None,
            Err(e) => {
                log::warn!("credential store read failed: {e}");
                None
            }
        }
    }

    fn set(&self, account: &str, secret: &str) {
        if let Err(e) = keyring::Entry::new(SERVICE, account).and_then(|e| e.set_password(secret)) {
            log::warn!("credential store write failed; session will not persist: {e}");
        }
    }

    fn delete(&self, account: &str) {
        match keyring::Entry::new(SERVICE, account).and_then(|e| e.delete_credential()) {
            Ok(()) | Err(keyring::Error::NoEntry) => {}
            Err(e) => log::warn!("credential store delete failed: {e}"),
        }
    }
}
