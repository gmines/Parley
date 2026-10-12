use std::path::{Path, PathBuf};
use std::process::Command;

fn main() {
    stamp_commit();
    build_voice();
    tauri_build::build()
}

/// Stamps the git commit into the binary for the About page. CI provides
/// GITHUB_SHA; local builds ask git (and mark uncommitted changes).
fn stamp_commit() {
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
}

fn git(args: &[&str]) -> Option<String> {
    let out = Command::new("git").args(args).output().ok()?;
    out.status.success().then(|| String::from_utf8_lossy(&out.stdout).trim().to_string())
}

/// Compiles the voice shim (voice/parley_voice.c) and links pjproject + Opus
/// from the prefix built by scripts/build-voice-deps.sh.
fn build_voice() {
    println!("cargo:rerun-if-changed=voice/parley_voice.c");
    println!("cargo:rerun-if-changed=voice/parley_voice.h");
    println!("cargo:rerun-if-env-changed=PARLEY_VOICE_DEPS");

    let prefix = voice_prefix();
    let stamp = prefix.join(".stamp");
    if !stamp.exists() {
        panic!(
            "\n\nVoice libraries not found in {}.\nBuild them first: client/scripts/build-voice-deps.sh\n(or point PARLEY_VOICE_DEPS at an existing build)\n\n",
            prefix.display()
        );
    }
    println!("cargo:rerun-if-changed={}", stamp.display());

    if std::env::var("CARGO_CFG_TARGET_OS").as_deref() == Ok("windows") {
        return link_voice_windows(&prefix);
    }

    // pjproject installs a pkg-config file listing its libraries in link order.
    let pc_path = prefix.join("lib/pkgconfig");
    let pc = |args: &[&str]| -> String {
        let out = Command::new("pkg-config")
            .args(args)
            .arg("libpjproject")
            .env("PKG_CONFIG_PATH", &pc_path)
            .output()
            .expect("run pkg-config");
        assert!(out.status.success(), "pkg-config libpjproject failed: {}", String::from_utf8_lossy(&out.stderr));
        String::from_utf8(out.stdout).unwrap()
    };

    let mut shim = cc::Build::new();
    shim.file("voice/parley_voice.c").include(prefix.join("include")).warnings(false);
    for flag in pc(&["--cflags"]).split_whitespace() {
        if let Some(def) = flag.strip_prefix("-D") {
            let (k, v) = def.split_once('=').map_or((def, None), |(k, v)| (k, Some(v)));
            shim.define(k, v);
        } else if let Some(dir) = flag.strip_prefix("-I") {
            shim.include(dir);
        }
    }
    shim.compile("parley_voice");

    let libdir = prefix.join("lib");
    println!("cargo:rustc-link-search=native={}", libdir.display());
    for flag in pc(&["--static", "--libs"]).split_whitespace() {
        if let Some(dir) = flag.strip_prefix("-L") {
            if Path::new(dir).is_dir() {
                println!("cargo:rustc-link-search=native={dir}");
            }
        } else if let Some(name) = flag.strip_prefix("-l") {
            // Our own static builds (pj*, opus) link statically; system
            // libraries (ssl, asound, m, pthread, ...) dynamically.
            let kind = if libdir.join(format!("lib{name}.a")).exists() { "static" } else { "dylib" };
            println!("cargo:rustc-link-lib={kind}={name}");
        } else if flag.starts_with("-framework") {
            // handled with the following token on macOS
        } else if let Some(fw) = flag.strip_prefix("-Wl,-framework,") {
            println!("cargo:rustc-link-lib=framework={fw}");
        }
    }
    // The WebRTC echo canceller bundled with pjproject is C++.
    match std::env::var("CARGO_CFG_TARGET_OS").as_deref() {
        Ok("linux") => println!("cargo:rustc-link-lib=dylib=stdc++"),
        Ok("macos") => println!("cargo:rustc-link-lib=dylib=c++"),
        _ => {}
    }
    if std::env::var("CARGO_CFG_TARGET_OS").as_deref() == Ok("macos") {
        for fw in ["CoreAudio", "AudioToolbox", "AudioUnit", "CoreFoundation", "CoreServices", "Security"] {
            println!("cargo:rustc-link-lib=framework={fw}");
        }
    }
}

/// Windows: build-voice-deps.ps1 produces one combined pjproject.lib (MSBuild
/// has no pkg-config file), plus opus.lib.
fn link_voice_windows(prefix: &Path) {
    cc::Build::new()
        .file("voice/parley_voice.c")
        .include(prefix.join("include"))
        .define("PJ_WIN64", "1")
        .warnings(false)
        .compile("parley_voice");
    println!("cargo:rustc-link-search=native={}", prefix.join("lib").display());
    for lib in ["pjproject", "opus"] {
        println!("cargo:rustc-link-lib=static={lib}");
    }
    // Sockets, audio (WMME / DirectSound / WASAPI), COM, IP helper.
    for lib in ["ws2_32", "iphlpapi", "winmm", "dsound", "ole32", "oleaut32", "uuid", "advapi32", "user32", "ksuser"] {
        println!("cargo:rustc-link-lib=dylib={lib}");
    }
}

fn voice_prefix() -> PathBuf {
    if let Ok(p) = std::env::var("PARLEY_VOICE_DEPS") {
        return PathBuf::from(p);
    }
    let os = match std::env::var("CARGO_CFG_TARGET_OS").unwrap().as_str() {
        "macos" => "darwin".to_string(),
        other => other.to_string(),
    };
    let arch = match (os.as_str(), std::env::var("CARGO_CFG_TARGET_ARCH").unwrap().as_str()) {
        ("darwin", "aarch64") => "arm64".to_string(),
        (_, a) => a.to_string(),
    };
    Path::new(&std::env::var("CARGO_MANIFEST_DIR").unwrap()).join("../.deps").join(format!("{os}-{arch}"))
}
