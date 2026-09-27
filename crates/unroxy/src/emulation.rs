//! A rotating browser profile for outbound requests.
//!
//! The Go server used uTLS with `HelloChrome_Auto` for provider fetches and
//! for the proxy transport. Rotating among a few current browser profiles
//! keeps the same property, a modern ClientHello per connection, without
//! pinning the port to one browser build.

use std::sync::atomic::{AtomicUsize, Ordering};

use wreq_util::Profile;

const PROFILES: [Profile; 6] = [
    Profile::Chrome136,
    Profile::Chrome131,
    Profile::Edge134,
    Profile::Firefox135,
    Profile::Safari26,
    Profile::OkHttp5,
];

static NEXT: AtomicUsize = AtomicUsize::new(0);

/// Rotates through current browser profiles so every client build sends a
/// different, plausible ClientHello.
pub fn next() -> Profile {
    let index = NEXT.fetch_add(1, Ordering::Relaxed) % PROFILES.len();
    PROFILES[index]
}
