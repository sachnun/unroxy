use std::path::PathBuf;
use std::process::Command;

fn main() {
    let manifest = PathBuf::from(std::env::var("CARGO_MANIFEST_DIR").unwrap());
    let out = PathBuf::from(std::env::var("OUT_DIR").unwrap());
    let lib = out.join("libpsiphon.a");

    let src = manifest.join("go");
    println!("cargo:rerun-if-changed={}", src.display());
    println!("cargo:rerun-if-changed=build.rs");

    // psiphon-tls reads Go runtime internals and checks the layout of
    // tls.ConnectionState at init, so the archive must be built with the Go
    // release the core was pinned to. Newer toolchains abort at startup.
    let toolchain = std::env::var("UNROXY_GO_TOOLCHAIN").unwrap_or_else(|_| "go1.26.0".to_string());

    if std::env::var_os("UNROXY_SKIP_GO").is_none()
        && std::env::var_os("UNROXY_PSIPHON_LIB_DIR").is_none()
    {
        let status = Command::new("go")
            .current_dir(&src)
            .env("GOFLAGS", "-mod=mod")
            .env("GOTOOLCHAIN", toolchain)
            .args([
                "build",
                "-buildmode=c-archive",
                "-tags",
                "PSIPHON_DISABLE_INPROXY PSIPHON_DISABLE_QUIC PSIPHON_DISABLE_GQUIC",
                "-o",
            ])
            .arg(&lib)
            .arg(".")
            .status()
            .expect("failed to run go build; install Go 1.26+ or set UNROXY_SKIP_GO");
        assert!(status.success(), "go build failed");
    }

    // A prebuilt archive can be supplied instead of running the Go build, so
    // the Docker image can link the one its Go stage produced.
    let search =
        std::env::var("UNROXY_PSIPHON_LIB_DIR").unwrap_or_else(|_| out.display().to_string());
    println!("cargo:rustc-link-search=native={search}");
    println!("cargo:rustc-link-lib=static=psiphon");
    for lib in ["pthread", "resolv", "dl", "m", "rt"] {
        println!("cargo:rustc-link-lib=dylib={lib}");
    }
    println!("cargo:include={}", out.display());
}
