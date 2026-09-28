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

pub fn next() -> Profile {
    let index = NEXT.fetch_add(1, Ordering::Relaxed) % PROFILES.len();
    PROFILES[index]
}
