use std::cell::RefCell;
use std::mem::size_of;
use std::sync::{
    Arc, Mutex, OnceLock,
    atomic::{AtomicBool, Ordering},
};
use windows::Win32::UI::Input::KeyboardAndMouse::*;
use windows::Win32::UI::WindowsAndMessaging::SetCursorPos;

#[derive(Clone, serde::Serialize)]
pub struct Journal {
    pub key: u16,
    pub scan: u16,
    pub flags: u32,
    pub button: String,
    pub down: bool,
}
static INPUTS: OnceLock<Mutex<Vec<Journal>>> = OnceLock::new();
thread_local! {static CANCEL:RefCell<Option<Arc<AtomicBool>>>=const{RefCell::new(None)};}
pub fn set_cancel(cancel: Arc<AtomicBool>) {
    CANCEL.with(|value| *value.borrow_mut() = Some(cancel))
}
pub fn check_cancel() -> Result<(), String> {
    if CANCEL.with(|value| {
        value
            .borrow()
            .as_ref()
            .is_some_and(|value| value.load(Ordering::SeqCst))
    }) {
        Err("computer operation cancelled".into())
    } else {
        Ok(())
    }
}
fn journal(value: Journal, inputs: &mut Vec<Journal>) {
    if value.down {
        inputs.push(value.clone())
    } else {
        inputs.retain(|item| {
            !(item.key == value.key
                && item.scan == value.scan
                && item.flags & 4 == value.flags & 4
                && item.button == value.button)
        })
    }
    super::emit_event(serde_json::json!({"event":"input","input":value}));
}
pub fn move_to(point: (i32, i32)) -> Result<(), String> {
    check_cancel()?;
    unsafe { SetCursorPos(point.0, point.1) }.map_err(super::error_string)
}
pub fn mouse_button(button: &str, down: bool) -> Result<(), String> {
    let button = if button.is_empty() { "left" } else { button };
    let flags = match (button, down) {
        ("right", true) => MOUSEEVENTF_RIGHTDOWN,
        ("right", false) => MOUSEEVENTF_RIGHTUP,
        ("middle", true) => MOUSEEVENTF_MIDDLEDOWN,
        ("middle", false) => MOUSEEVENTF_MIDDLEUP,
        (_, true) => MOUSEEVENTF_LEFTDOWN,
        _ => MOUSEEVENTF_LEFTUP,
    };
    let mut inputs = INPUTS
        .get_or_init(|| Mutex::new(Vec::new()))
        .lock()
        .map_err(super::error_string)?;
    if down {
        check_cancel()?;
        journal(
            Journal {
                key: 0,
                scan: 0,
                flags: 0,
                button: button.into(),
                down,
            },
            &mut inputs,
        )
    }
    let result = send_mouse(flags, 0);
    if (!down && result.is_ok()) || (down && result.is_err()) {
        journal(
            Journal {
                key: 0,
                scan: 0,
                flags: 0,
                button: button.into(),
                down: false,
            },
            &mut inputs,
        )
    }
    result
}
pub fn send_mouse(flags: MOUSE_EVENT_FLAGS, data: u32) -> Result<(), String> {
    let input = INPUT {
        r#type: INPUT_MOUSE,
        Anonymous: INPUT_0 {
            mi: MOUSEINPUT {
                mouseData: data,
                dwFlags: flags,
                ..Default::default()
            },
        },
    };
    if unsafe { SendInput(&[input], size_of::<INPUT>() as i32) } != 1 {
        return Err("SendInput rejected mouse input (possibly UIPI)".into());
    }
    Ok(())
}
fn raw_key(key: u16, scan: u16, flags: KEYBD_EVENT_FLAGS) -> Result<(), String> {
    let input = INPUT {
        r#type: INPUT_KEYBOARD,
        Anonymous: INPUT_0 {
            ki: KEYBDINPUT {
                wVk: VIRTUAL_KEY(key),
                wScan: scan,
                dwFlags: flags,
                ..Default::default()
            },
        },
    };
    if unsafe { SendInput(&[input], size_of::<INPUT>() as i32) } != 1 {
        return Err("SendInput rejected keyboard input (possibly UIPI)".into());
    }
    Ok(())
}
fn send_key(key: u16, scan: u16, flags: KEYBD_EVENT_FLAGS) -> Result<(), String> {
    let down = flags.0 & KEYEVENTF_KEYUP.0 == 0;
    let mut inputs = INPUTS
        .get_or_init(|| Mutex::new(Vec::new()))
        .lock()
        .map_err(super::error_string)?;
    if down {
        check_cancel()?;
        journal(
            Journal {
                key,
                scan,
                flags: flags.0,
                button: String::new(),
                down,
            },
            &mut inputs,
        )
    }
    let result = raw_key(key, scan, flags);
    if (!down && result.is_ok()) || (down && result.is_err()) {
        journal(
            Journal {
                key,
                scan,
                flags: flags.0,
                button: String::new(),
                down: false,
            },
            &mut inputs,
        )
    }
    result
}
pub fn release_all() -> bool {
    let Ok(mut inputs) = INPUTS.get_or_init(|| Mutex::new(Vec::new())).lock() else {
        return false;
    };
    let mut failed = Vec::new();
    while let Some(mut value) = inputs.pop() {
        value.down = false;
        let result = if value.button.is_empty() {
            raw_key(
                value.key,
                value.scan,
                KEYBD_EVENT_FLAGS(value.flags | KEYEVENTF_KEYUP.0),
            )
        } else {
            let flags = match value.button.as_str() {
                "right" => MOUSEEVENTF_RIGHTUP,
                "middle" => MOUSEEVENTF_MIDDLEUP,
                _ => MOUSEEVENTF_LEFTUP,
            };
            send_mouse(flags, 0)
        };
        if result.is_ok() {
            super::emit_event(serde_json::json!({"event":"input","input":value}));
        } else {
            value.down = true;
            failed.push(value);
        }
    }
    *inputs = failed;
    inputs.is_empty()
}
pub fn type_text(value: &str) -> Result<(), String> {
    if value.is_empty() {
        return Err("text cannot be empty".into());
    }
    for unit in value.encode_utf16() {
        send_key(0, unit, KEYEVENTF_UNICODE)?;
        send_key(0, unit, KEYEVENTF_UNICODE | KEYEVENTF_KEYUP)?;
    }
    Ok(())
}
pub fn press_key(value: &str) -> Result<(), String> {
    let keys = value
        .trim()
        .to_uppercase()
        .split('+')
        .map(|part| virtual_key(part).ok_or_else(|| format!("unsupported key {part:?}")))
        .collect::<Result<Vec<_>, _>>()?;
    if keys.is_empty() {
        return Err("key cannot be empty".into());
    }
    for key in &keys {
        send_key(*key, 0, KEYBD_EVENT_FLAGS(0))?;
    }
    for key in keys.iter().rev() {
        send_key(*key, 0, KEYEVENTF_KEYUP)?;
    }
    Ok(())
}
fn virtual_key(value: &str) -> Option<u16> {
    Some(match value {
        "BACKSPACE" => 0x08,
        "TAB" => 0x09,
        "ENTER" => 0x0D,
        "SHIFT" => 0x10,
        "CTRL" | "CONTROL" => 0x11,
        "ALT" => 0x12,
        "ESC" | "ESCAPE" => 0x1B,
        "SPACE" => 0x20,
        "PAGEUP" => 0x21,
        "PAGEDOWN" => 0x22,
        "END" => 0x23,
        "HOME" => 0x24,
        "LEFT" => 0x25,
        "UP" => 0x26,
        "RIGHT" => 0x27,
        "DOWN" => 0x28,
        "DELETE" => 0x2E,
        "META" | "WIN" => 0x5B,
        one if one.len() == 1 && one.as_bytes()[0].is_ascii_alphanumeric() => {
            one.as_bytes()[0] as u16
        }
        function if function.starts_with('F') => {
            let value = function[1..].parse::<u16>().ok()?;
            if !(1..=24).contains(&value) {
                return None;
            }
            0x6F + value
        }
        _ => return None,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn keys_are_bounded() {
        assert_eq!(virtual_key("F24"), Some(0x87));
        assert_eq!(virtual_key("F25"), None);
        assert_eq!(virtual_key(""), None);
    }
    #[test]
    fn cooperative_cancel_prevents_dispatch() {
        set_cancel(Arc::new(AtomicBool::new(true)));
        assert!(check_cancel().is_err());
    }
}
