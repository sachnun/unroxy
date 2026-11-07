use std::sync::atomic::{AtomicU32, Ordering};

pub struct ReplayWindow {
    last: AtomicU32,
    bits: std::sync::Mutex<u64>,
    window: u32,
}

impl ReplayWindow {
    pub fn new() -> Self {
        Self {
            last: AtomicU32::new(0),
            bits: std::sync::Mutex::new(0),
            window: 64,
        }
    }

    pub fn accept(&self, id: u32) -> bool {
        let last = self.last.load(Ordering::Relaxed);
        if id == 0 {
            return false;
        }
        if id > last {
            let shift = id - last;
            let mut bits = self.bits.lock().expect("replay lock");
            if shift >= self.window {
                *bits = 0;
            } else {
                *bits <<= shift;
            }
            *bits |= 1;
            self.last.store(id, Ordering::Relaxed);
            return true;
        }
        let behind = last - id;
        if behind >= self.window {
            return false;
        }
        let mut bits = self.bits.lock().expect("replay lock");
        let mask = 1u64 << behind;
        if *bits & mask != 0 {
            return false;
        }
        *bits |= mask;
        true
    }
}

impl Default for ReplayWindow {
    fn default() -> Self {
        Self::new()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn accepts_in_order_and_rejects_duplicates() {
        let window = ReplayWindow::new();
        assert!(window.accept(1));
        assert!(window.accept(2));
        assert!(!window.accept(2));
        assert!(window.accept(4));
        assert!(window.accept(3));
        assert!(!window.accept(3));
    }

    #[test]
    fn rejects_too_old() {
        let window = ReplayWindow::new();
        assert!(window.accept(1));
        assert!(window.accept(1000));
        assert!(!window.accept(2));
    }
}
