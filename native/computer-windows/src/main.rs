#![allow(unsafe_op_in_unsafe_fn)]

use std::collections::HashMap;
use std::io::{self, Read, Write};
use std::mem::size_of;
use std::sync::{Arc, Mutex, mpsc};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use base64::Engine as _;
use image::{GenericImage, ImageEncoder, RgbaImage, codecs::png::PngEncoder};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use windows::Win32::Foundation::{CloseHandle, FILETIME, HWND, POINT, RECT};
use windows::Win32::Graphics::Gdi::{GetMonitorInfoW, HMONITOR, MONITORINFO};
use windows::Win32::System::Com::{
    CLSCTX_INPROC_SERVER, COINIT_MULTITHREADED, CoCreateInstance, CoInitializeEx, CoUninitialize,
};
use windows::Win32::System::Threading::{
    AttachThreadInput, GetCurrentThreadId, GetProcessTimes, OpenProcess,
    PROCESS_QUERY_LIMITED_INFORMATION,
};
use windows::Win32::UI::Accessibility::{
    CUIAutomation, IUIAutomation, UIA_ButtonControlTypeId, UIA_CheckBoxControlTypeId,
    UIA_ComboBoxControlTypeId, UIA_EditControlTypeId, UIA_HyperlinkControlTypeId,
    UIA_ListItemControlTypeId, UIA_MenuItemControlTypeId, UIA_RadioButtonControlTypeId,
    UIA_SliderControlTypeId, UIA_TabItemControlTypeId, UIA_TreeItemControlTypeId,
};
use windows::Win32::UI::HiDpi::{
    DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2, SetProcessDpiAwarenessContext,
};
use windows::Win32::UI::Input::KeyboardAndMouse::*;
use windows::Win32::UI::WindowsAndMessaging::{
    BringWindowToTop, GetCursorPos, GetForegroundWindow, GetWindowThreadProcessId, IsIconic,
    IsWindow, IsWindowVisible, SW_RESTORE, SetCursorPos, SetForegroundWindow, ShowWindow,
    SwitchToThisWindow,
};
use windows_capture::capture::{Context, GraphicsCaptureApiHandler};
use windows_capture::frame::Frame;
use windows_capture::graphics_capture_api::InternalCaptureControl;
use windows_capture::monitor::Monitor;
use windows_capture::settings::{
    ColorFormat, CursorCaptureSettings, DirtyRegionSettings, DrawBorderSettings,
    MinimumUpdateIntervalSettings, SecondaryWindowSettings, Settings,
};
use windows_capture::window::Window as CaptureWindow;

const VERSION: &str = "9.0.2";
const MAX_STATES: usize = 64;
const MAX_ELEMENTS: usize = 500;

#[derive(Deserialize)]
struct Request {
    id: u64,
    method: String,
    #[serde(default)]
    params: Value,
}

#[derive(Serialize)]
struct Response {
    id: u64,
    #[serde(skip_serializing_if = "Option::is_none")]
    result: Option<Value>,
    #[serde(skip_serializing_if = "Option::is_none")]
    error: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    image_base64: Option<String>,
}

#[derive(Clone, Copy, Debug, Default, Deserialize, PartialEq, Serialize)]
struct Rectangle {
    x: i32,
    y: i32,
    width: i32,
    height: i32,
}

#[derive(Serialize)]
struct Target {
    target_id: String,
    pid: u32,
    title: String,
    bounds: Rectangle,
    active: bool,
}

#[derive(Clone, Serialize)]
struct Element {
    ref_id: String,
    role: String,
    name: String,
    bounds: Rectangle,
    disabled: bool,
}

#[derive(Serialize)]
struct StateOutput {
    state_id: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    target_id: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    title: String,
    origin_x: i32,
    origin_y: i32,
    width: i32,
    height: i32,
    cursor_x: i32,
    cursor_y: i32,
    mime_type: &'static str,
    elements: Vec<Element>,
    elements_truncated: bool,
}

#[derive(Deserialize)]
struct StateParams {
    #[serde(default)]
    target_id: String,
}

#[derive(Deserialize)]
struct ActionParams {
    kind: String,
    #[serde(default)]
    target_id: String,
    #[serde(default)]
    state_id: String,
    #[serde(default)]
    element_ref: String,
    x: Option<i32>,
    y: Option<i32>,
    to_x: Option<i32>,
    to_y: Option<i32>,
    #[serde(default)]
    button: String,
    #[serde(default)]
    text: String,
    #[serde(default)]
    key: String,
    #[serde(default)]
    scroll_x: i32,
    #[serde(default)]
    scroll_y: i32,
}

#[derive(Clone)]
struct StateRecord {
    epoch: u64,
    target_id: String,
    foreground: isize,
    bounds: Rectangle,
    elements: HashMap<String, Rectangle>,
}

struct Engine {
    nonce: u128,
    epoch: u64,
    sequence: u64,
    states: HashMap<String, StateRecord>,
}

impl Engine {
    fn new() -> Self {
        Self {
            nonce: SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .unwrap_or_default()
                .as_nanos(),
            epoch: 1,
            sequence: 0,
            states: HashMap::new(),
        }
    }

    fn execute(&mut self, request: &Request) -> Result<(Value, Option<Vec<u8>>), String> {
        match request.method.as_str() {
            "hello" => Ok((
                json!({"version": VERSION, "backend": "windows-rust-wgc-uia"}),
                None,
            )),
            "targets" => Ok((
                serde_json::to_value(self.targets()?).map_err(error_string)?,
                None,
            )),
            "state" => {
                let params: StateParams =
                    serde_json::from_value(request.params.clone()).map_err(error_string)?;
                let (state, image) = self.state(params)?;
                Ok((
                    serde_json::to_value(state).map_err(error_string)?,
                    Some(image),
                ))
            }
            "act" => {
                let params: ActionParams =
                    serde_json::from_value(request.params.clone()).map_err(error_string)?;
                self.act(params)?;
                Ok((json!({"success": true}), None))
            }
            _ => Err(format!(
                "unsupported computer worker method {:?}",
                request.method
            )),
        }
    }

    fn targets(&self) -> Result<Vec<Target>, String> {
        let foreground = unsafe { GetForegroundWindow() }.0 as isize;
        let mut output = Vec::new();
        for window in CaptureWindow::enumerate().map_err(error_string)? {
            let title = window.title().map_err(error_string)?;
            let pid = window.process_id().map_err(error_string)?;
            let rect = window.rect().map_err(error_string)?;
            let bounds = rectangle(rect);
            if title.trim().is_empty() || bounds.width <= 0 || bounds.height <= 0 {
                continue;
            }
            let raw = window.as_raw_hwnd() as isize;
            let started = match process_start(pid) {
                Ok(value) => value,
                Err(_) => continue,
            };
            output.push(Target {
                target_id: target_id(raw, pid, started),
                pid,
                title,
                bounds,
                active: raw == foreground,
            });
        }
        output.sort_by_key(|target| (!target.active, target.title.to_lowercase()));
        Ok(output)
    }

    fn state(&mut self, params: StateParams) -> Result<(StateOutput, Vec<u8>), String> {
        let (target, title, bounds) = if params.target_id.is_empty() {
            (None, String::new(), desktop_bounds()?)
        } else {
            let window = resolve_target(&params.target_id)?;
            require_foreground(window)?;
            let capture = CaptureWindow::from_raw_hwnd(window.0);
            (
                Some(window),
                capture.title().map_err(error_string)?,
                rectangle(capture.rect().map_err(error_string)?),
            )
        };
        let foreground_before = unsafe { GetForegroundWindow() }.0 as isize;
        let (image, width, height) = match target {
            Some(window) => capture_item(CaptureWindow::from_raw_hwnd(window.0))?,
            None => capture_desktop(bounds)?,
        };
        let foreground_after = unsafe { GetForegroundWindow() }.0 as isize;
        if foreground_before != foreground_after {
            return Err(
                "foreground window changed during capture; call computer_state again".into(),
            );
        }
        let uia_window = target.unwrap_or(HWND(foreground_after as *mut _));
        let (mut elements, elements_truncated) =
            collect_accessibility(uia_window, bounds, MAX_ELEMENTS);
        self.sequence += 1;
        let state_id = format!("c{:x}-{}-{}", self.nonce, self.epoch, self.sequence);
        let mut element_map = HashMap::new();
        for (index, element) in elements.iter_mut().enumerate() {
            element.ref_id = format!("{}:u{}", state_id, index + 1);
            element_map.insert(element.ref_id.clone(), element.bounds);
        }
        let mut cursor = POINT::default();
        unsafe {
            let _ = GetCursorPos(&mut cursor);
        }
        let state = StateOutput {
            state_id: state_id.clone(),
            target_id: params.target_id.clone(),
            title,
            origin_x: bounds.x,
            origin_y: bounds.y,
            width,
            height,
            cursor_x: cursor.x - bounds.x,
            cursor_y: cursor.y - bounds.y,
            mime_type: "image/png",
            elements,
            elements_truncated,
        };
        if self.states.len() >= MAX_STATES {
            self.states.clear();
        }
        self.states.insert(
            state_id,
            StateRecord {
                epoch: self.epoch,
                target_id: params.target_id,
                foreground: foreground_after,
                bounds,
                elements: element_map,
            },
        );
        Ok((state, image))
    }

    fn act(&mut self, params: ActionParams) -> Result<(), String> {
        if params.kind == "activate" {
            if params.target_id.is_empty() || !params.state_id.is_empty() {
                return Err("activate requires target_id and does not accept state_id".into());
            }
            activate(resolve_target(&params.target_id)?)?;
            self.invalidate();
            return Ok(());
        }
        if params.state_id.is_empty() {
            return Err(
                "state_id is required; call computer_state immediately before acting".into(),
            );
        }
        let record = self
            .states
            .get(&params.state_id)
            .cloned()
            .ok_or("state_id is unknown or stale; call computer_state again")?;
        if record.epoch != self.epoch || record.target_id != params.target_id {
            return Err("target_id does not match the referenced state".into());
        }
        let foreground = unsafe { GetForegroundWindow() }.0 as isize;
        if foreground != record.foreground {
            return Err("foreground window changed; call computer_state again".into());
        }
        if !params.target_id.is_empty() {
            let current = rectangle(
                CaptureWindow::from_raw_hwnd(resolve_target(&params.target_id)?.0)
                    .rect()
                    .map_err(error_string)?,
            );
            if current != record.bounds {
                return Err("target bounds changed; call computer_state again".into());
            }
        }
        perform_action(&params, &record)?;
        self.invalidate();
        Ok(())
    }

    fn invalidate(&mut self) {
        self.epoch += 1;
        self.states.clear();
    }
}

#[derive(Clone)]
struct Captured {
    data: Vec<u8>,
    width: i32,
    height: i32,
}

struct OneFrame {
    output: Arc<Mutex<Option<Result<Captured, String>>>>,
}

impl GraphicsCaptureApiHandler for OneFrame {
    type Flags = Arc<Mutex<Option<Result<Captured, String>>>>;
    type Error = String;
    fn new(ctx: Context<Self::Flags>) -> Result<Self, Self::Error> {
        Ok(Self { output: ctx.flags })
    }
    fn on_frame_arrived(
        &mut self,
        frame: &mut Frame,
        control: InternalCaptureControl,
    ) -> Result<(), Self::Error> {
        let result = (|| {
            let width = frame.width();
            let height = frame.height();
            if width == 0 || height == 0 {
                return Err(format!("WGC returned an empty {width}x{height} frame"));
            }
            let buffer = frame
                .buffer()
                .map_err(|error| format!("map WGC {width}x{height} frame: {error}"))?;
            let mut packed = Vec::new();
            let raw = buffer.as_nopadding_buffer(&mut packed);
            let mut encoded = Vec::new();
            PngEncoder::new(&mut encoded)
                .write_image(raw, width, height, image::ExtendedColorType::Rgba8)
                .map_err(|error| {
                    format!(
                        "encode WGC {width}x{height} frame with {} bytes: {error}",
                        raw.len()
                    )
                })?;
            Ok(Captured {
                data: encoded,
                width: width as i32,
                height: height as i32,
            })
        })();
        *self.output.lock().map_err(error_string)? = Some(result);
        control.stop();
        Ok(())
    }
}

fn capture_item<T>(item: T) -> Result<(Vec<u8>, i32, i32), String>
where
    T: TryInto<windows_capture::settings::GraphicsCaptureItemType>,
{
    let output = Arc::new(Mutex::new(None));
    let settings = Settings::new(
        item,
        CursorCaptureSettings::WithCursor,
        DrawBorderSettings::WithoutBorder,
        SecondaryWindowSettings::Include,
        MinimumUpdateIntervalSettings::Default,
        DirtyRegionSettings::Default,
        ColorFormat::Rgba8,
        output.clone(),
    );
    OneFrame::start(settings).map_err(|error| format!("start WGC one-frame capture: {error}"))?;
    let captured = output
        .lock()
        .map_err(error_string)?
        .take()
        .ok_or("capture ended without a frame")??;
    Ok((captured.data, captured.width, captured.height))
}

fn capture_desktop(bounds: Rectangle) -> Result<(Vec<u8>, i32, i32), String> {
    let mut desktop = RgbaImage::new(bounds.width as u32, bounds.height as u32);
    for monitor in Monitor::enumerate().map_err(error_string)? {
        let rect = monitor_rect(monitor.as_raw_hmonitor())?;
        let (encoded, width, height) = capture_item(monitor).map_err(|error| {
            format!(
                "capture monitor {}x{} at {},{}: {error}",
                rect.width, rect.height, rect.x, rect.y
            )
        })?;
        let frame = image::load_from_memory(&encoded)
            .map_err(error_string)?
            .into_rgba8();
        desktop.copy_from(&frame, (rect.x - bounds.x) as u32, (rect.y - bounds.y) as u32).map_err(|error| format!("compose captured {width}x{height} monitor into {}x{} desktop at {},{}: {error}", bounds.width, bounds.height, rect.x-bounds.x, rect.y-bounds.y))?;
        if width != rect.width || height != rect.height {
            return Err("monitor capture dimensions changed during capture".into());
        }
    }
    let mut encoded = Vec::new();
    PngEncoder::new(&mut encoded)
        .write_image(
            desktop.as_raw(),
            desktop.width(),
            desktop.height(),
            image::ExtendedColorType::Rgba8,
        )
        .map_err(|error| {
            format!(
                "encode composed {}x{} desktop: {error}",
                bounds.width, bounds.height
            )
        })?;
    Ok((encoded, bounds.width, bounds.height))
}

fn collect_accessibility(window: HWND, origin: Rectangle, limit: usize) -> (Vec<Element>, bool) {
    let (sender, receiver) = mpsc::channel();
    let raw_window = window.0 as isize;
    std::thread::spawn(move || {
        let result =
            unsafe { collect_accessibility_inner(HWND(raw_window as *mut _), origin, limit) };
        let _ = sender.send(result);
    });
    receiver
        .recv_timeout(Duration::from_secs(2))
        .unwrap_or_default()
}

unsafe fn collect_accessibility_inner(
    window: HWND,
    origin: Rectangle,
    limit: usize,
) -> (Vec<Element>, bool) {
    if CoInitializeEx(None, COINIT_MULTITHREADED).is_err() {
        return (Vec::new(), false);
    }
    let result = (|| -> windows::core::Result<(Vec<Element>, bool)> {
        let automation: IUIAutomation =
            CoCreateInstance(&CUIAutomation, None, CLSCTX_INPROC_SERVER)?;
        let root = automation.ElementFromHandle(window)?;
        let walker = automation.ControlViewWalker()?;
        let mut stack = Vec::new();
        if let Ok(first) = walker.GetFirstChildElement(&root) {
            stack.push(first);
        }
        let mut elements = Vec::new();
        let mut visited = 0usize;
        let mut truncated = false;
        while let Some(element) = stack.pop() {
            visited += 1;
            if visited > 5_000 {
                truncated = true;
                break;
            }
            if let Ok(sibling) = walker.GetNextSiblingElement(&element) {
                stack.push(sibling);
            }
            if let Ok(child) = walker.GetFirstChildElement(&element) {
                stack.push(child);
            }
            let control_type = match element.CurrentControlType() {
                Ok(value) => value,
                Err(_) => continue,
            };
            if !interactive_control(control_type.0) {
                continue;
            }
            let rect = match element.CurrentBoundingRectangle() {
                Ok(value) => rectangle(value),
                Err(_) => continue,
            };
            if rect.width <= 0 || rect.height <= 0 {
                continue;
            }
            let name = element
                .CurrentName()
                .map(|value| value.to_string())
                .unwrap_or_default();
            elements.push(Element {
                ref_id: String::new(),
                role: role_name(control_type.0).into(),
                name,
                bounds: Rectangle {
                    x: rect.x - origin.x,
                    y: rect.y - origin.y,
                    width: rect.width,
                    height: rect.height,
                },
                disabled: element
                    .CurrentIsEnabled()
                    .map(|value| !value.as_bool())
                    .unwrap_or(false),
            });
            if elements.len() == limit {
                truncated = !stack.is_empty();
                break;
            }
        }
        Ok((elements, truncated))
    })()
    .unwrap_or_default();
    CoUninitialize();
    result
}

fn interactive_control(value: i32) -> bool {
    matches!(
        value,
        50000 | 50002 | 50003 | 50004 | 50005 | 50007 | 50011 | 50013 | 50015 | 50019 | 50024
    )
}
fn role_name(value: i32) -> &'static str {
    if value == UIA_ButtonControlTypeId.0 {
        "button"
    } else if value == UIA_CheckBoxControlTypeId.0 {
        "checkbox"
    } else if value == UIA_ComboBoxControlTypeId.0 {
        "combobox"
    } else if value == UIA_EditControlTypeId.0 {
        "textbox"
    } else if value == UIA_HyperlinkControlTypeId.0 {
        "link"
    } else if value == UIA_ListItemControlTypeId.0 {
        "listitem"
    } else if value == UIA_MenuItemControlTypeId.0 {
        "menuitem"
    } else if value == UIA_RadioButtonControlTypeId.0 {
        "radio"
    } else if value == UIA_SliderControlTypeId.0 {
        "slider"
    } else if value == UIA_TabItemControlTypeId.0 {
        "tab"
    } else if value == UIA_TreeItemControlTypeId.0 {
        "treeitem"
    } else {
        "control"
    }
}

fn perform_action(params: &ActionParams, record: &StateRecord) -> Result<(), String> {
    let point = if !params.element_ref.is_empty() {
        let rect = record
            .elements
            .get(&params.element_ref)
            .ok_or("element_ref is unknown or stale")?;
        Some((
            record.bounds.x + rect.x + rect.width / 2,
            record.bounds.y + rect.y + rect.height / 2,
        ))
    } else if let (Some(x), Some(y)) = (params.x, params.y) {
        if x < 0 || y < 0 || x >= record.bounds.width || y >= record.bounds.height {
            return Err("coordinates are outside the referenced state".into());
        }
        Some((record.bounds.x + x, record.bounds.y + y))
    } else {
        None
    };
    match params.kind.as_str() {
        "move" => move_to(point.ok_or("move requires coordinates or element_ref")?),
        "click" | "double_click" => {
            move_to(point.ok_or("click requires coordinates or element_ref")?)?;
            let count = if params.kind == "double_click" { 2 } else { 1 };
            for _ in 0..count {
                mouse_button(&params.button, true)?;
                mouse_button(&params.button, false)?;
            }
            Ok(())
        }
        "drag" => {
            let start = point.ok_or("drag requires coordinates or element_ref")?;
            let (x, y) = (
                params.to_x.ok_or("drag requires to_x")?,
                params.to_y.ok_or("drag requires to_y")?,
            );
            if x < 0 || y < 0 || x >= record.bounds.width || y >= record.bounds.height {
                return Err("drag destination is outside the referenced state".into());
            }
            let end = (record.bounds.x + x, record.bounds.y + y);
            move_to(start)?;
            mouse_button(&params.button, true)?;
            for step in 1..=20 {
                move_to((
                    start.0 + (end.0 - start.0) * step / 20,
                    start.1 + (end.1 - start.1) * step / 20,
                ))?;
                std::thread::sleep(Duration::from_millis(10));
            }
            mouse_button(&params.button, false)
        }
        "type_text" | "set_value" => {
            if let Some(value) = point {
                move_to(value)?;
                mouse_button("left", true)?;
                mouse_button("left", false)?;
            }
            if params.kind == "set_value" {
                press_key("CTRL+A")?;
            }
            type_text(&params.text)
        }
        "press_key" => {
            if let Some(value) = point {
                move_to(value)?;
                mouse_button("left", true)?;
                mouse_button("left", false)?;
            }
            press_key(&params.key)
        }
        "scroll" => {
            if let Some(value) = point {
                move_to(value)?;
            }
            if params.scroll_y != 0 {
                send_mouse(MOUSEEVENTF_WHEEL, (-params.scroll_y) as u32)?;
            }
            if params.scroll_x != 0 {
                send_mouse(MOUSEEVENTF_HWHEEL, params.scroll_x as u32)?;
            }
            Ok(())
        }
        _ => Err(format!("unsupported computer action {:?}", params.kind)),
    }
}

fn move_to(point: (i32, i32)) -> Result<(), String> {
    unsafe {
        SetCursorPos(point.0, point.1)
            .ok()
            .ok_or_else(|| "SetCursorPos failed".into())
    }
}
fn mouse_button(button: &str, down: bool) -> Result<(), String> {
    let flags = match (button, down) {
        ("right", true) => MOUSEEVENTF_RIGHTDOWN,
        ("right", false) => MOUSEEVENTF_RIGHTUP,
        ("middle", true) => MOUSEEVENTF_MIDDLEDOWN,
        ("middle", false) => MOUSEEVENTF_MIDDLEUP,
        (_, true) => MOUSEEVENTF_LEFTDOWN,
        _ => MOUSEEVENTF_LEFTUP,
    };
    send_mouse(flags, 0)
}
fn send_mouse(flags: MOUSE_EVENT_FLAGS, data: u32) -> Result<(), String> {
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
        return Err("SendInput rejected mouse input (possibly UIPI integrity isolation)".into());
    }
    Ok(())
}
fn send_key(key: u16, scan: u16, flags: KEYBD_EVENT_FLAGS) -> Result<(), String> {
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
        return Err("SendInput rejected keyboard input (possibly UIPI integrity isolation)".into());
    }
    Ok(())
}
fn type_text(value: &str) -> Result<(), String> {
    if value.is_empty() {
        return Err("text cannot be empty".into());
    }
    for unit in value.encode_utf16() {
        send_key(0, unit, KEYEVENTF_UNICODE)?;
        send_key(0, unit, KEYEVENTF_UNICODE | KEYEVENTF_KEYUP)?;
    }
    Ok(())
}
fn press_key(value: &str) -> Result<(), String> {
    let parts: Vec<_> = value
        .trim()
        .to_uppercase()
        .split('+')
        .map(str::to_owned)
        .collect();
    if parts.is_empty() || parts[0].is_empty() {
        return Err("key cannot be empty".into());
    }
    let mut keys = Vec::new();
    for part in parts {
        keys.push(virtual_key(&part).ok_or_else(|| format!("unsupported key {part:?}"))?);
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
        one if one.len() == 1 => one.as_bytes()[0] as u16,
        function if function.starts_with('F') => 0x6F + function[1..].parse::<u16>().ok()?,
        _ => return None,
    })
}

fn activate(window: HWND) -> Result<(), String> {
    unsafe {
        if !IsWindow(Some(window)).as_bool() || !IsWindowVisible(window).as_bool() {
            return Err("target window is no longer available".into());
        }
        if IsIconic(window).as_bool() {
            let _ = ShowWindow(window, SW_RESTORE);
        }
        let _ = BringWindowToTop(window);
        let mut requested = SetForegroundWindow(window).as_bool();
        if wait_for_foreground(window, 10) {
            return Ok(());
        }

        let foreground = GetForegroundWindow();
        let current_thread = GetCurrentThreadId();
        let foreground_thread = GetWindowThreadProcessId(foreground, None);
        let target_thread = GetWindowThreadProcessId(window, None);
        let attached_foreground = foreground_thread != 0
            && foreground_thread != current_thread
            && AttachThreadInput(current_thread, foreground_thread, true).as_bool();
        let attached_target = target_thread != 0
            && target_thread != current_thread
            && target_thread != foreground_thread
            && AttachThreadInput(current_thread, target_thread, true).as_bool();
        let _ = BringWindowToTop(window);
        requested = SetForegroundWindow(window).as_bool() || requested;
        let _ = SetFocus(Some(window));
        let activated_while_attached = wait_for_foreground(window, 15);
        if attached_target {
            let _ = AttachThreadInput(current_thread, target_thread, false);
        }
        if attached_foreground {
            let _ = AttachThreadInput(current_thread, foreground_thread, false);
        }
        if activated_while_attached {
            return Ok(());
        }

        SwitchToThisWindow(window, true);
        if wait_for_foreground(window, 20) {
            return Ok(());
        }
        if requested {
            Err("window did not become foreground within 450 ms".into())
        } else {
            Err("Windows rejected the foreground activation request".into())
        }
    }
}
unsafe fn wait_for_foreground(window: HWND, attempts: usize) -> bool {
    for _ in 0..attempts {
        if unsafe { GetForegroundWindow() } == window {
            return true;
        }
        std::thread::sleep(Duration::from_millis(10));
    }
    false
}
fn require_foreground(window: HWND) -> Result<(), String> {
    if unsafe { GetForegroundWindow() } != window {
        Err("selected target is not foreground; activate it, then call computer_state again".into())
    } else {
        Ok(())
    }
}
fn target_id(hwnd: isize, pid: u32, started: u64) -> String {
    format!("win-{hwnd:x}-{pid:x}-{started:x}")
}
fn resolve_target(value: &str) -> Result<HWND, String> {
    let fields: Vec<_> = value.split('-').collect();
    if fields.len() != 4 || fields[0] != "win" {
        return Err("target_id is invalid; call computer_targets again".into());
    }
    let raw = isize::from_str_radix(fields[1], 16).map_err(|_| "target_id is invalid")?;
    let expected_pid = u32::from_str_radix(fields[2], 16).map_err(|_| "target_id is invalid")?;
    let expected_start = u64::from_str_radix(fields[3], 16).map_err(|_| "target_id is invalid")?;
    let window = HWND(raw as *mut _);
    let mut pid = 0;
    unsafe {
        if !IsWindow(Some(window)).as_bool() || !IsWindowVisible(window).as_bool() {
            return Err("target window is no longer available".into());
        }
        GetWindowThreadProcessId(window, Some(&mut pid));
    }
    if pid != expected_pid || process_start(pid)? != expected_start {
        return Err("target_id is stale; call computer_targets again".into());
    }
    Ok(window)
}
fn process_start(pid: u32) -> Result<u64, String> {
    let process = unsafe { OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, false, pid) }
        .map_err(error_string)?;
    let mut created = FILETIME::default();
    let mut exited = FILETIME::default();
    let mut kernel = FILETIME::default();
    let mut user = FILETIME::default();
    let result =
        unsafe { GetProcessTimes(process, &mut created, &mut exited, &mut kernel, &mut user) }
            .map_err(error_string);
    unsafe {
        let _ = CloseHandle(process);
    }
    result?;
    Ok(((created.dwHighDateTime as u64) << 32) | created.dwLowDateTime as u64)
}
fn rectangle(value: RECT) -> Rectangle {
    Rectangle {
        x: value.left,
        y: value.top,
        width: value.right - value.left,
        height: value.bottom - value.top,
    }
}
fn monitor_rect(raw: *mut std::ffi::c_void) -> Result<Rectangle, String> {
    let mut info = MONITORINFO {
        cbSize: size_of::<MONITORINFO>() as u32,
        ..Default::default()
    };
    unsafe {
        GetMonitorInfoW(HMONITOR(raw), &mut info)
            .ok()
            .map_err(error_string)?;
    }
    Ok(rectangle(info.rcMonitor))
}
fn desktop_bounds() -> Result<Rectangle, String> {
    let mut bounds: Option<Rectangle> = None;
    for monitor in Monitor::enumerate().map_err(error_string)? {
        let rect = monitor_rect(monitor.as_raw_hmonitor())?;
        bounds = Some(match bounds {
            None => rect,
            Some(old) => {
                let left = old.x.min(rect.x);
                let top = old.y.min(rect.y);
                let right = (old.x + old.width).max(rect.x + rect.width);
                let bottom = (old.y + old.height).max(rect.y + rect.height);
                Rectangle {
                    x: left,
                    y: top,
                    width: right - left,
                    height: bottom - top,
                }
            }
        });
    }
    bounds.ok_or("no active monitor was found".into())
}
fn error_string(error: impl std::fmt::Display) -> String {
    error.to_string()
}

fn read_request(reader: &mut impl Read) -> io::Result<Option<Request>> {
    let mut header = [0u8; 4];
    match reader.read_exact(&mut header) {
        Ok(()) => {}
        Err(error) if error.kind() == io::ErrorKind::UnexpectedEof => return Ok(None),
        Err(error) => return Err(error),
    }
    let length = u32::from_le_bytes(header) as usize;
    if length == 0 || length > 16 * 1024 * 1024 {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "invalid request frame length",
        ));
    }
    let mut data = vec![0; length];
    reader.read_exact(&mut data)?;
    serde_json::from_slice(&data)
        .map(Some)
        .map_err(|error| io::Error::new(io::ErrorKind::InvalidData, error))
}
fn write_response(writer: &mut impl Write, response: &Response) -> io::Result<()> {
    let data = serde_json::to_vec(response).map_err(io::Error::other)?;
    writer.write_all(&(data.len() as u32).to_le_bytes())?;
    writer.write_all(&data)?;
    writer.flush()
}

fn main() -> io::Result<()> {
    unsafe {
        let _ = SetProcessDpiAwarenessContext(DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2);
    }
    let mut engine = Engine::new();
    let mut input = io::stdin().lock();
    let mut output = io::stdout().lock();
    while let Some(request) = read_request(&mut input)? {
        let response = match engine.execute(&request) {
            Ok((result, image)) => Response {
                id: request.id,
                result: Some(result),
                error: None,
                image_base64: image
                    .map(|data| base64::engine::general_purpose::STANDARD.encode(data)),
            },
            Err(error) => Response {
                id: request.id,
                result: None,
                error: Some(error),
                image_base64: None,
            },
        };
        write_response(&mut output, &response)?;
    }
    Ok(())
}
