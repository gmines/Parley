use std::process::Command;

fn main() {
    // Stamp the git commit into the binary for the About page. CI provides
    // GITHUB_SHA; local builds ask git (and mark uncommitted changes).
    let commit = std::env::var("GITHUB_SHA")
        .ok()
        .map(|sha| sha.chars().take(12).collect::<String>())
        .or_else(|| {
            let sha = git(&["rev-parse", "--short=12", "HEAD"])?;
            let dirty = git(&["status", "--porcelain"]).is_some_and(|s| !s.is_empty());
            Some(if dirty { format!("{sha}-dirty") } else { sha })
        })
        .unwrap_or_else(|| "unknown".into());
    println!("cargo:rustc-env=PARLEY_COMMIT={commit}");
    println!("cargo:rerun-if-env-changed=GITHUB_SHA");
    println!("cargo:rerun-if-changed=../../.git/HEAD");
    println!("cargo:rerun-if-changed=../../.git/index");

    tauri_build::build()
}

fn git(args: &[&str]) -> Option<String> {
    let out = Command::new("git").args(args).output().ok()?;
    out.status.success().then(|| String::from_utf8_lossy(&out.stdout).trim().to_string())
}
