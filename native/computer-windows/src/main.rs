#![allow(unsafe_op_in_unsafe_fn)]

use std::collections::{HashMap, VecDeque};
use std::io::{self, Read, Write};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex, mpsc};
use std::sync::{OnceLock, atomic::AtomicU64};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use base64::Engine as _;
use image::codecs::png::{CompressionType, FilterType as PngFilterType, PngEncoder};
use image::{GenericImage, ImageEncoder, RgbaImage};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use windows::Win32::Foundation::{CloseHandle, FILETIME, HWND, LPARAM, POINT, RECT};
use windows::Win32::Graphics::Dwm::{
    DWMWA_CLOAKED, DWMWA_EXTENDED_FRAME_BOUNDS, DwmGetWindowAttribute,
};
use windows::Win32::Graphics::Gdi::{
    EnumDisplayMonitors, GetMonitorInfoW, HDC, HMONITOR, MONITOR_DEFAULTTONEAREST, MONITORINFO,
    MONITORINFOEXW, MonitorFromWindow,
};
use windows::Win32::System::Threading::{
    AttachThreadInput, GetCurrentThreadId, GetProcessTimes, OpenProcess, PROCESS_NAME_WIN32,
    PROCESS_QUERY_LIMITED_INFORMATION, QueryFullProcessImageNameW,
};
use windows::Win32::UI::HiDpi::{
    DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2, GetDpiForMonitor, MDT_EFFECTIVE_DPI,
    SetProcessDpiAwarenessContext,
};
use windows::Win32::UI::Input::KeyboardAndMouse::*;
use windows::Win32::UI::WindowsAndMessaging::{
    BringWindowToTop, EnumWindows, GWL_EXSTYLE, GetClassNameW, GetCursorPos, GetForegroundWindow,
    GetWindowLongPtrW, GetWindowRect, GetWindowTextW, GetWindowThreadProcessId, IsIconic, IsWindow,
    IsWindowVisible, IsZoomed, SW_RESTORE, SW_SHOWMAXIMIZED, SW_SHOWMINNOACTIVE, SW_SHOWNOACTIVATE,
    SWP_NOACTIVATE, SWP_NOOWNERZORDER, SWP_NOZORDER, SetForegroundWindow, SetWindowPos, ShowWindow,
    SwitchToThisWindow, WS_EX_TOOLWINDOW, WS_EX_TOPMOST,
};
use windows::core::BOOL;
use windows_capture::capture::{Context, GraphicsCaptureApiHandler};
use windows_capture::frame::Frame;
use windows_capture::graphics_capture_api::InternalCaptureControl;
use windows_capture::monitor::Monitor;
use windows_capture::settings::{
    ColorFormat, CursorCaptureSettings, DirtyRegionSettings, DrawBorderSettings,
    MinimumUpdateIntervalSettings, SecondaryWindowSettings, Settings,
};
use windows_capture::window::Window as CaptureWindow;

mod input;
mod overlay;
mod uia;
use input::{check_cancel, mouse_button, move_to, press_key, send_mouse, type_text};

const VERSION: &str = "13.0.0";
const MAX_STATES: usize = 256;
const DEFAULT_ELEMENTS: usize = 300;
const MAX_ELEMENTS: usize = 5000;

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

/// Physical pixels in the virtual desktop, or relative to an observation origin where stated.
#[derive(Clone, Copy, Debug, Default, Deserialize, PartialEq, Serialize)]
struct Rectangle {
    x: i32,
    y: i32,
    width: i32,
    height: i32,
}

impl Rectangle {
    fn contains(&self, x: i32, y: i32) -> bool {
        x >= self.x && y >= self.y && x < self.x + self.width && y < self.y + self.height
    }
}

#[derive(Clone, Serialize)]
struct Display {
    display_id: String,
    bounds: Rectangle,
    work_area: Rectangle,
    scale: f32,
    primary: bool,
    #[serde(skip)]
    handle: isize,
}

#[derive(Serialize)]
struct Target {
    target_id: String,
    pid: u32,
    process: String,
    title: String,
    class_name: String,
    bounds: Rectangle,
    state: &'static str,
    display_id: String,
    active: bool,
    /// Tool windows (widgets, palettes, floating cards) stay out of the taskbar and Alt+Tab.
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    tool: bool,
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    topmost: bool,
}

#[derive(Serialize)]
struct Targets {
    displays: Vec<Display>,
    windows: Vec<Target>,
}

#[derive(Deserialize, Default)]
struct TargetsParams {
    #[serde(default)]
    pid: u32,
    #[serde(default)]
    title: String,
    #[serde(default)]
    process: String,
}

#[derive(Clone, Serialize)]
struct Element {
    ref_id: String,
    role: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    name: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    automation_id: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    class_name: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    value: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    bounds: Option<Rectangle>,
    depth: u32,
    #[serde(skip_serializing_if = "Option::is_none")]
    parent: Option<u32>,
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    disabled: bool,
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    offscreen: bool,
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    focused: bool,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    patterns: Vec<&'static str>,
    #[serde(skip)]
    native_ref: u64,
}

#[derive(Serialize)]
struct StateOutput {
    state_id: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    target_id: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    display_id: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    title: String,
    origin_x: i32,
    origin_y: i32,
    width: i32,
    height: i32,
    scale: f32,
    cursor_x: i32,
    cursor_y: i32,
    cursor_shape: &'static str,
    has_image: bool,
    mime_type: &'static str,
    elements: Vec<Element>,
    elements_total: usize,
    elements_truncated: bool,
    actionable: bool,
    uia_status: String,
}

fn yes() -> bool {
    true
}

#[derive(Deserialize)]
struct StateParams {
    #[serde(default)]
    control_id: String,
    #[serde(default)]
    target_id: String,
    #[serde(default)]
    display_id: String,
    region: Option<Rectangle>,
    #[serde(default = "yes")]
    image: bool,
    #[serde(default)]
    cursor: bool,
    #[serde(default)]
    elements: String,
    #[serde(default)]
    name: String,
    #[serde(default)]
    automation_id: String,
    #[serde(default)]
    role: String,
    #[serde(default)]
    max_elements: usize,
    #[serde(default)]
    offset: usize,
    #[serde(default)]
    max_width: u32,
    #[serde(default)]
    max_height: u32,
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
    duration_ms: u32,
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
    bounds: Option<Rectangle>,
    #[serde(default)]
    show: String,
}

#[derive(Clone)]
struct StateRecord {
    stop_generation: u64,
    epoch: u64,
    target_id: String,
    foreground: isize,
    bounds: Rectangle,
    frame: Option<Rectangle>,
    actionable: bool,
    elements: HashMap<String, u64>,
}

struct Engine {
    nonce: u128,
    epoch: u64,
    sequence: u64,
    states: HashMap<String, StateRecord>,
    state_order: VecDeque<String>,
    uia: uia::Worker,
    overlay: overlay::Overlay,
}

const SEMANTIC: [&str; 8] = [
    "invoke",
    "toggle",
    "select",
    "expand",
    "collapse",
    "set_value",
    "scroll_into_view",
    "scroll_by",
];

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
            state_order: VecDeque::new(),
            uia: uia::Worker::new(),
            overlay: overlay::Overlay::new(),
        }
    }

    fn execute(&mut self, request: &Request) -> Result<(Value, Option<Vec<u8>>), String> {
        match request.method.as_str() {
            "hello" => Ok((
                json!({"version": VERSION, "backend": "windows-rust-wgc-uia"}),
                None,
            )),
            "targets" => {
                let params: TargetsParams = if request.params.is_null() {
                    TargetsParams::default()
                } else {
                    serde_json::from_value(request.params.clone()).map_err(error_string)?
                };
                Ok((
                    serde_json::to_value(targets(&params)?).map_err(error_string)?,
                    None,
                ))
            }
            "state" => {
                let params: StateParams =
                    serde_json::from_value(request.params.clone()).map_err(error_string)?;
                let (state, image) = self.state(params)?;
                Ok((serde_json::to_value(state).map_err(error_string)?, image))
            }
            "act" => {
                let params: ActionParams =
                    serde_json::from_value(request.params.clone()).map_err(error_string)?;
                self.act(params)?;
                Ok((json!({"success": true}), None))
            }
            "show_control" => {
                let label = request.params["label"].as_str().unwrap_or("");
                self.invalidate();
                self.overlay.show(label)?;
                Ok((json!({"success":true}), None))
            }
            "invalidate" => {
                self.invalidate();
                Ok((json!({"success":true}), None))
            }
            _ => Err(format!(
                "unsupported computer worker method {:?}",
                request.method
            )),
        }
    }

    fn state(&mut self, params: StateParams) -> Result<(StateOutput, Option<Vec<u8>>), String> {
        check_cancel()?;
        let actionable = !params.control_id.is_empty();
        if !params.target_id.is_empty() && !params.display_id.is_empty() {
            return Err("observe a target_id or a display_id, not both".into());
        }
        let scope = match params.elements.as_str() {
            "" | "all" => "all",
            "interactive" => "interactive",
            "none" => "none",
            other => {
                return Err(format!(
                    "elements must be all, interactive or none, not {other:?}"
                ));
            }
        };
        let limit = match params.max_elements {
            0 => DEFAULT_ELEMENTS,
            value if value <= MAX_ELEMENTS => value,
            _ => return Err(format!("max_elements must be at most {MAX_ELEMENTS}")),
        };
        let displays = displays()?;
        let (window, display, title, area) = if !params.target_id.is_empty() {
            let window = resolve_target(&params.target_id)?;
            if actionable {
                require_foreground(window)?;
            }
            let bounds = frame_bounds(window);
            let display = display_of(&displays, window);
            (Some(window), display, window_title(window), bounds)
        } else if !params.display_id.is_empty() {
            let display = displays
                .iter()
                .find(|display| display.display_id == params.display_id)
                .ok_or("display_id is unknown; call desktop.targets() again")?;
            (None, Some(display.clone()), String::new(), display.bounds)
        } else {
            (None, None, String::new(), virtual_bounds(&displays)?)
        };
        let foreground_before = unsafe { GetForegroundWindow() }.0 as isize;
        let image = if params.image {
            let frame = match (window, &display) {
                (Some(window), _) => {
                    if unsafe { IsIconic(window) }.as_bool() {
                        return Err(
                            "target is minimized; desktop.action({ kind: \"place\", show: \"restore\", target_id }) brings it back without activating it"
                                .into(),
                        );
                    }
                    capture_frame(CaptureWindow::from_raw_hwnd(window.0), params.cursor)?
                }
                (None, Some(display)) => capture_display(display, params.cursor)?,
                (None, None) => capture_desktop(&displays, area, params.cursor)?,
            };
            Some(frame)
        } else {
            None
        };
        // Window captures are of the visible frame; trust the captured size over the queried one.
        let mut origin = area;
        if let Some(frame) = &image {
            origin.width = frame.width() as i32;
            origin.height = frame.height() as i32;
        }
        let (image, origin) = match params.region {
            Some(region) => {
                if region.width <= 0
                    || region.height <= 0
                    || region.x < 0
                    || region.y < 0
                    || region.x + region.width > origin.width
                    || region.y + region.height > origin.height
                {
                    return Err(format!(
                        "region must lie within the {}x{} observed area",
                        origin.width, origin.height
                    ));
                }
                let cropped = image.map(|frame| {
                    image::imageops::crop_imm(
                        &frame,
                        region.x as u32,
                        region.y as u32,
                        region.width as u32,
                        region.height as u32,
                    )
                    .to_image()
                });
                (
                    cropped,
                    Rectangle {
                        x: origin.x + region.x,
                        y: origin.y + region.y,
                        width: region.width,
                        height: region.height,
                    },
                )
            }
            None => (image, origin),
        };
        let (mut elements, total, uia_truncated, uia_status) = match (window, scope) {
            (_, "none") => (Vec::new(), 0, false, "not_requested".to_string()),
            (None, _) => (Vec::new(), 0, false, "window_target_required".to_string()),
            (Some(window), scope) => match self.uia.collect(
                window,
                origin,
                uia::Query {
                    scope: scope.into(),
                    name: params.name.clone(),
                    automation_id: params.automation_id.clone(),
                    role: params.role.clone(),
                    limit,
                    offset: params.offset,
                },
            ) {
                Ok(collected) => (
                    collected.elements,
                    collected.total,
                    collected.truncated,
                    "available".to_string(),
                ),
                Err(error) => (Vec::new(), 0, false, error),
            },
        };
        check_cancel()?;
        let foreground_after = unsafe { GetForegroundWindow() }.0 as isize;
        if actionable && foreground_before != foreground_after {
            return Err("foreground changed during observation; observe again".into());
        }
        self.sequence += 1;
        let state_id = format!("c{:x}-{}-{}", self.nonce, self.epoch, self.sequence);
        let mut element_map = HashMap::new();
        for (index, element) in elements.iter_mut().enumerate() {
            element.ref_id = format!("{}:e{}", state_id, params.offset + index + 1);
            element_map.insert(element.ref_id.clone(), element.native_ref);
        }
        let mut cursor = POINT::default();
        unsafe {
            let _ = GetCursorPos(&mut cursor);
        }
        let cursor_shape = cursor_shape();
        let truncated = uia_truncated || params.offset + elements.len() < total;
        let encoded = match &image {
            Some(frame) => Some(encode_png(frame, params.max_width, params.max_height)?),
            None => None,
        };
        let state = StateOutput {
            state_id: state_id.clone(),
            target_id: params.target_id.clone(),
            display_id: display
                .as_ref()
                .map(|display| display.display_id.clone())
                .unwrap_or_default(),
            title,
            origin_x: origin.x,
            origin_y: origin.y,
            width: origin.width,
            height: origin.height,
            scale: display.as_ref().map(|display| display.scale).unwrap_or(1.0),
            cursor_x: cursor.x - origin.x,
            cursor_y: cursor.y - origin.y,
            cursor_shape,
            has_image: encoded.is_some(),
            mime_type: "image/png",
            elements,
            elements_total: total,
            elements_truncated: truncated,
            actionable,
            uia_status,
        };
        while self.states.len() >= MAX_STATES {
            if let Some(oldest) = self.state_order.pop_front() {
                self.states.remove(&oldest);
            } else {
                self.states.clear();
                break;
            }
        }
        self.state_order.push_back(state_id.clone());
        self.states.insert(
            state_id,
            StateRecord {
                stop_generation: STOP_GENERATION.load(Ordering::SeqCst),
                epoch: self.epoch,
                target_id: params.target_id,
                foreground: foreground_after,
                bounds: origin,
                frame: window.map(frame_bounds),
                actionable,
                elements: element_map,
            },
        );
        if actionable {
            self.overlay.near(origin);
        }
        Ok((state, encoded))
    }

    fn act(&mut self, params: ActionParams) -> Result<(), String> {
        check_cancel()?;
        if params.kind == "place" {
            let window = resolve_target(&params.target_id)?;
            return place_window(window, params.bounds, &params.show);
        }
        if SEMANTIC.contains(&params.kind.as_str()) {
            return self.semantic(&params);
        }
        if params.kind == "activate" {
            if params.target_id.is_empty() || !params.state_id.is_empty() {
                return Err("activate requires target_id and does not accept state_id".into());
            }
            let window = resolve_target(&params.target_id)?;
            self.invalidate();
            activate(window)?;
            self.overlay.near(frame_bounds(window));
            return Ok(());
        }
        if params.state_id.is_empty() {
            return Err(
                "state_id is required; call desktop.state({ control_id, ... }) immediately before acting".into(),
            );
        }
        let record = self
            .states
            .get(&params.state_id)
            .cloned()
            .ok_or("state_id is unknown or stale; call desktop.state() again")?;
        if !record.actionable {
            return Err(
                "physical input needs a state observed with control_id; semantic actions do not"
                    .into(),
            );
        }
        if record.epoch != self.epoch
            || record.target_id != params.target_id
            || record.stop_generation != STOP_GENERATION.load(Ordering::SeqCst)
        {
            return Err("target_id does not match the referenced state".into());
        }
        let foreground = unsafe { GetForegroundWindow() }.0 as isize;
        if foreground != record.foreground {
            return Err("foreground window changed; call desktop.state() again".into());
        }
        if !params.target_id.is_empty()
            && Some(frame_bounds(resolve_target(&params.target_id)?)) != record.frame
        {
            return Err("target bounds changed; call desktop.state() again".into());
        }
        self.invalidate();
        let live = if params.element_ref.is_empty() {
            None
        } else {
            let key = *record
                .elements
                .get(&params.element_ref)
                .ok_or("element_ref unknown or stale")?;
            Some(self.uia.bounds(key)?)
        };
        check_cancel()?;
        if unsafe { GetForegroundWindow() }.0 as isize != record.foreground {
            return Err("foreground changed during element resolution; observe again".into());
        }
        let result = perform_action(&params, &record, live);
        if !input::release_all() {
            return Err("injected input cleanup incomplete; release desktop control".into());
        }
        result
    }

    /// Operates a control through its automation pattern: no foreground, pointer or keyboard.
    fn semantic(&mut self, params: &ActionParams) -> Result<(), String> {
        if params.element_ref.is_empty() {
            return Err(format!("{} requires element_ref", params.kind));
        }
        let state_id = params
            .element_ref
            .rsplit_once(':')
            .map(|(state, _)| state)
            .ok_or("element_ref is invalid")?;
        let record = self
            .states
            .get(state_id)
            .ok_or("element_ref belongs to an expired observation; observe again")?;
        if record.stop_generation != STOP_GENERATION.load(Ordering::SeqCst) {
            return Err("desktop control was stopped locally; observe again".into());
        }
        let key = *record
            .elements
            .get(&params.element_ref)
            .ok_or("element_ref unknown or stale")?;
        let action = match params.kind.as_str() {
            "invoke" => uia::Semantic::Invoke,
            "toggle" => uia::Semantic::Toggle,
            "select" => uia::Semantic::Select,
            "expand" => uia::Semantic::Expand,
            "collapse" => uia::Semantic::Collapse,
            "set_value" => uia::Semantic::SetValue(params.text.clone()),
            "scroll_into_view" => uia::Semantic::ScrollIntoView,
            "scroll_by" => {
                if params.scroll_x == 0 && params.scroll_y == 0 {
                    return Err("scroll_by needs scroll_x or scroll_y pages".into());
                }
                uia::Semantic::ScrollBy(params.scroll_x, params.scroll_y)
            }
            _ => unreachable!(),
        };
        self.uia.semantic(key, action)
    }

    fn invalidate(&mut self) {
        self.epoch += 1;
        self.states.clear();
        self.state_order.clear();
    }
}

struct OneFrame {
    output: Arc<Mutex<Option<Result<RgbaImage, String>>>>,
}

impl GraphicsCaptureApiHandler for OneFrame {
    type Flags = Arc<Mutex<Option<Result<RgbaImage, String>>>>;
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
            RgbaImage::from_raw(width, height, raw.to_vec())
                .ok_or_else(|| format!("WGC {width}x{height} frame has an unexpected size"))
        })();
        *self.output.lock().map_err(error_string)? = Some(result);
        control.stop();
        Ok(())
    }
}

fn capture_frame<T>(item: T, cursor: bool) -> Result<RgbaImage, String>
where
    T: TryInto<windows_capture::settings::GraphicsCaptureItemType>,
{
    let output = Arc::new(Mutex::new(None));
    let settings = Settings::new(
        item,
        if cursor {
            CursorCaptureSettings::WithCursor
        } else {
            CursorCaptureSettings::WithoutCursor
        },
        DrawBorderSettings::WithoutBorder,
        SecondaryWindowSettings::Include,
        MinimumUpdateIntervalSettings::Default,
        DirtyRegionSettings::Default,
        ColorFormat::Rgba8,
        output.clone(),
    );
    OneFrame::start(settings).map_err(|error| format!("start WGC one-frame capture: {error}"))?;
    output
        .lock()
        .map_err(error_string)?
        .take()
        .ok_or("capture ended without a frame")?
}

fn capture_display(display: &Display, cursor: bool) -> Result<RgbaImage, String> {
    let monitor = Monitor::enumerate()
        .map_err(error_string)?
        .into_iter()
        .find(|monitor| monitor.as_raw_hmonitor() as isize == display.handle)
        .ok_or("display disappeared; call computer_targets again")?;
    capture_frame(monitor, cursor)
}

fn capture_desktop(
    displays: &[Display],
    area: Rectangle,
    cursor: bool,
) -> Result<RgbaImage, String> {
    let mut desktop = RgbaImage::new(area.width as u32, area.height as u32);
    for display in displays {
        let frame = capture_display(display, cursor)?;
        let rect = display.bounds;
        if frame.width() as i32 != rect.width || frame.height() as i32 != rect.height {
            return Err("display size changed during capture; observe again".into());
        }
        desktop
            .copy_from(&frame, (rect.x - area.x) as u32, (rect.y - area.y) as u32)
            .map_err(error_string)?;
    }
    Ok(desktop)
}

fn encode_png(frame: &RgbaImage, max_width: u32, max_height: u32) -> Result<Vec<u8>, String> {
    let resized;
    let src = if (max_width > 0 && frame.width() > max_width)
        || (max_height > 0 && frame.height() > max_height)
    {
        let mut scale = 1.0_f64;
        if max_width > 0 && frame.width() > max_width {
            scale = scale.min(max_width as f64 / frame.width() as f64);
        }
        if max_height > 0 && frame.height() > max_height {
            scale = scale.min(max_height as f64 / frame.height() as f64);
        }
        let nw = ((frame.width() as f64 * scale).round() as u32).max(1);
        let nh = ((frame.height() as f64 * scale).round() as u32).max(1);
        resized = image::imageops::resize(frame, nw, nh, image::imageops::FilterType::Triangle);
        &resized
    } else {
        frame
    };

    // Strip opaque alpha channel (RGBA -> RGB24) to cut raw scanline payload by 25%,
    // then encode with Fast compression + Sub filter (~5x faster than Default+Adaptive).
    let w = src.width();
    let h = src.height();
    let raw = src.as_raw();
    let mut rgb = Vec::with_capacity((w as usize) * (h as usize) * 3);
    for px in raw.chunks_exact(4) {
        rgb.extend_from_slice(&px[..3]);
    }

    let mut encoded = Vec::with_capacity(rgb.len() / 6);
    PngEncoder::new_with_quality(&mut encoded, CompressionType::Fast, PngFilterType::Sub)
        .write_image(&rgb, w, h, image::ExtendedColorType::Rgb8)
        .map_err(|error| format!("encode {w}x{h} PNG: {error}"))?;
    Ok(encoded)
}

fn perform_action(
    params: &ActionParams,
    record: &StateRecord,
    live: Option<Rectangle>,
) -> Result<(), String> {
    let point = if !params.element_ref.is_empty() {
        let rect = live.ok_or("element_ref is unknown or stale")?;
        let center = (rect.x + rect.width / 2, rect.y + rect.height / 2);
        if !record.bounds.contains(center.0, center.1) {
            return Err("UIA element is outside the observed area; observe again".into());
        }
        Some(center)
    } else if let (Some(x), Some(y)) = (params.x, params.y) {
        if x < 0 || y < 0 || x >= record.bounds.width || y >= record.bounds.height {
            return Err("coordinates are outside the referenced state".into());
        }
        Some((record.bounds.x + x, record.bounds.y + y))
    } else {
        None
    };
    check_cancel()?;
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
            // The pointer moves every eight milliseconds or so across the drag's duration, as a hand does, so what
            // follows it (a window, a slider) is seen as it would be under a person's drag.
            let duration = if params.duration_ms == 0 {
                200
            } else {
                params.duration_ms
            } as i32;
            let steps = (duration / 8).max(20);
            for step in 1..=steps {
                move_to((
                    start.0 + (end.0 - start.0) * step / steps,
                    start.1 + (end.1 - start.1) * step / steps,
                ))?;
                std::thread::sleep(Duration::from_millis((duration / steps).max(1) as u64));
            }
            mouse_button(&params.button, false)
        }
        "type_text" => {
            if let Some(value) = point {
                move_to(value)?;
                mouse_button("left", true)?;
                mouse_button("left", false)?;
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

/// Shows, restores, minimizes or moves a window without making it the foreground window.
/// Bounds are the visible frame, as computer_targets reports them.
fn place_window(window: HWND, bounds: Option<Rectangle>, show: &str) -> Result<(), String> {
    if bounds.is_none() && show.is_empty() {
        return Err("place needs bounds, show, or both".into());
    }
    unsafe {
        match show {
            "" => {}
            "restore" => {
                let _ = ShowWindow(window, SW_SHOWNOACTIVATE);
            }
            "minimize" => {
                let _ = ShowWindow(window, SW_SHOWMINNOACTIVE);
            }
            "maximize" => {
                let _ = ShowWindow(window, SW_SHOWMAXIMIZED);
            }
            other => {
                return Err(format!(
                    "show must be restore, minimize or maximize, not {other:?}"
                ));
            }
        }
        if let Some(target) = bounds {
            if target.width < 80 || target.height < 60 {
                return Err("bounds must be at least 80x60".into());
            }
            if IsIconic(window).as_bool() || IsZoomed(window).as_bool() {
                let _ = ShowWindow(window, SW_SHOWNOACTIVATE);
            }
            // A move across displays of different scale resizes the window once Windows
            // delivers WM_DPICHANGED; the second pass lands it on the requested frame.
            for _ in 0..2 {
                let mut outer = RECT::default();
                GetWindowRect(window, &mut outer).map_err(error_string)?;
                let frame = frame_bounds(window);
                let left = frame.x - outer.left;
                let top = frame.y - outer.top;
                let right = outer.right - (frame.x + frame.width);
                let bottom = outer.bottom - (frame.y + frame.height);
                SetWindowPos(
                    window,
                    None,
                    target.x - left,
                    target.y - top,
                    target.width + left + right,
                    target.height + top + bottom,
                    SWP_NOACTIVATE | SWP_NOZORDER | SWP_NOOWNERZORDER,
                )
                .map_err(error_string)?;
                std::thread::sleep(Duration::from_millis(60));
            }
        }
    }
    Ok(())
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
        check_cancel()?;

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
        check_cancel()?;

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
        if check_cancel().is_err() {
            return false;
        }
        if unsafe { GetForegroundWindow() } == window {
            return true;
        }
        std::thread::sleep(Duration::from_millis(10));
    }
    false
}
fn require_foreground(window: HWND) -> Result<(), String> {
    if unsafe { GetForegroundWindow() } != window {
        Err("physical control needs the target in the foreground; activate it, or use semantic actions which do not".into())
    } else {
        Ok(())
    }
}

fn targets(params: &TargetsParams) -> Result<Targets, String> {
    let displays = displays()?;
    let foreground = unsafe { GetForegroundWindow() }.0 as isize;
    let title_filter = params.title.to_lowercase();
    let process_filter = params.process.to_lowercase();
    let mut windows = Vec::new();
    for raw in top_level_windows() {
        let hwnd = HWND(raw as *mut _);
        if !unsafe { IsWindowVisible(hwnd) }.as_bool() || cloaked(hwnd) {
            continue;
        }
        let mut pid = 0u32;
        unsafe { GetWindowThreadProcessId(hwnd, Some(&mut pid)) };
        if pid == std::process::id() || (params.pid != 0 && pid != params.pid) {
            continue;
        }
        let title = window_title(hwnd);
        let bounds = frame_bounds(hwnd);
        if title.trim().is_empty() || bounds.width <= 1 || bounds.height <= 1 {
            continue;
        }
        if !title_filter.is_empty() && !title.to_lowercase().contains(&title_filter) {
            continue;
        }
        let process = process_name(pid);
        if !process_filter.is_empty() && !process.to_lowercase().contains(&process_filter) {
            continue;
        }
        let started = match process_start(pid) {
            Ok(value) => value,
            Err(_) => continue,
        };
        let extended = unsafe { GetWindowLongPtrW(hwnd, GWL_EXSTYLE) } as u32;
        let state = unsafe {
            if IsIconic(hwnd).as_bool() {
                "minimized"
            } else if IsZoomed(hwnd).as_bool() {
                "maximized"
            } else {
                "normal"
            }
        };
        windows.push(Target {
            target_id: target_id(raw, pid, started),
            pid,
            process,
            title,
            class_name: class_name(hwnd),
            bounds,
            state,
            display_id: display_of(&displays, hwnd)
                .map(|display| display.display_id)
                .unwrap_or_default(),
            active: raw == foreground,
            tool: extended & WS_EX_TOOLWINDOW.0 != 0,
            topmost: extended & WS_EX_TOPMOST.0 != 0,
        });
    }
    windows.sort_by_key(|target| {
        (
            !target.active,
            target.state == "minimized",
            target.title.to_lowercase(),
        )
    });
    Ok(Targets { displays, windows })
}

/// Every top-level window in z-order, tool windows included: widgets pinned to the desktop
/// are tool windows owned by its icon view, and screen capture libraries skip them.
fn top_level_windows() -> Vec<isize> {
    unsafe extern "system" fn collect(window: HWND, data: LPARAM) -> BOOL {
        unsafe { (*(data.0 as *mut Vec<isize>)).push(window.0 as isize) };
        BOOL(1)
    }
    let mut windows: Vec<isize> = Vec::new();
    unsafe {
        let _ = EnumWindows(
            Some(collect),
            LPARAM(&mut windows as *mut Vec<isize> as isize),
        );
    }
    windows
}

fn displays() -> Result<Vec<Display>, String> {
    unsafe extern "system" fn collect(
        monitor: HMONITOR,
        _: HDC,
        _: *mut RECT,
        data: LPARAM,
    ) -> BOOL {
        unsafe { (*(data.0 as *mut Vec<isize>)).push(monitor.0 as isize) };
        BOOL(1)
    }
    let mut handles: Vec<isize> = Vec::new();
    unsafe {
        EnumDisplayMonitors(
            None,
            None,
            Some(collect),
            LPARAM(&mut handles as *mut Vec<isize> as isize),
        )
        .ok()
        .map_err(error_string)?;
    }
    let mut output = Vec::new();
    for handle in handles {
        let monitor = HMONITOR(handle as *mut _);
        let mut info = MONITORINFOEXW::default();
        info.monitorInfo.cbSize = size_of::<MONITORINFOEXW>() as u32;
        if !unsafe { GetMonitorInfoW(monitor, &mut info.monitorInfo as *mut MONITORINFO) }.as_bool()
        {
            continue;
        }
        let device = String::from_utf16_lossy(&info.szDevice);
        let device = device.trim_end_matches('\0');
        let (mut dpi, mut unused) = (96u32, 96u32);
        let _ = unsafe { GetDpiForMonitor(monitor, MDT_EFFECTIVE_DPI, &mut dpi, &mut unused) };
        output.push(Display {
            display_id: device.trim_start_matches(r"\\.\").to_string(),
            bounds: rectangle(info.monitorInfo.rcMonitor),
            work_area: rectangle(info.monitorInfo.rcWork),
            scale: dpi as f32 / 96.0,
            primary: info.monitorInfo.dwFlags & 1 != 0,
            handle,
        });
    }
    if output.is_empty() {
        return Err("no active display; the session may be locked or headless".into());
    }
    output.sort_by_key(|display| (!display.primary, display.bounds.x));
    Ok(output)
}

fn display_of(displays: &[Display], window: HWND) -> Option<Display> {
    let monitor = unsafe { MonitorFromWindow(window, MONITOR_DEFAULTTONEAREST) }.0 as isize;
    displays
        .iter()
        .find(|display| display.handle == monitor)
        .cloned()
}

fn virtual_bounds(displays: &[Display]) -> Result<Rectangle, String> {
    let mut bounds: Option<Rectangle> = None;
    for display in displays {
        let rect = display.bounds;
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
    bounds.ok_or("no active display".into())
}

/// The visible frame, without the invisible resize borders Windows adds around top-level windows.
fn frame_bounds(window: HWND) -> Rectangle {
    let mut frame = RECT::default();
    let ok = unsafe {
        DwmGetWindowAttribute(
            window,
            DWMWA_EXTENDED_FRAME_BOUNDS,
            &mut frame as *mut RECT as *mut _,
            size_of::<RECT>() as u32,
        )
    }
    .is_ok();
    if !ok {
        let _ = unsafe { GetWindowRect(window, &mut frame) };
    }
    rectangle(frame)
}

fn cloaked(window: HWND) -> bool {
    let mut value = 0u32;
    unsafe {
        DwmGetWindowAttribute(
            window,
            DWMWA_CLOAKED,
            &mut value as *mut u32 as *mut _,
            size_of::<u32>() as u32,
        )
    }
    .is_ok()
        && value != 0
}

fn window_title(window: HWND) -> String {
    let mut buffer = [0u16; 512];
    let length = unsafe { GetWindowTextW(window, &mut buffer) };
    String::from_utf16_lossy(&buffer[..length.max(0) as usize])
}

fn class_name(window: HWND) -> String {
    let mut buffer = [0u16; 256];
    let length = unsafe { GetClassNameW(window, &mut buffer) };
    String::from_utf16_lossy(&buffer[..length.max(0) as usize])
}

fn process_name(pid: u32) -> String {
    let Ok(process) = (unsafe { OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, false, pid) })
    else {
        return String::new();
    };
    let mut buffer = [0u16; 1024];
    let mut size = buffer.len() as u32;
    let ok = unsafe {
        QueryFullProcessImageNameW(
            process,
            PROCESS_NAME_WIN32,
            windows::core::PWSTR(buffer.as_mut_ptr()),
            &mut size,
        )
    }
    .is_ok();
    unsafe {
        let _ = CloseHandle(process);
    }
    if !ok {
        return String::new();
    }
    let path = String::from_utf16_lossy(&buffer[..size as usize]);
    path.rsplit('\\').next().unwrap_or_default().to_string()
}

fn target_id(hwnd: isize, pid: u32, started: u64) -> String {
    format!("win-{hwnd:x}-{pid:x}-{started:x}")
}
fn resolve_target(value: &str) -> Result<HWND, String> {
    let fields: Vec<_> = value.split('-').collect();
    if fields.len() != 4 || fields[0] != "win" {
        return Err("target_id is invalid; call desktop.targets() again".into());
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
        return Err("target_id is stale; call desktop.targets() again".into());
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

static OUTPUT: OnceLock<Mutex<io::Stdout>> = OnceLock::new();
type ActiveRequest = Option<(u64, Arc<AtomicBool>)>;
static ACTIVE: OnceLock<Mutex<ActiveRequest>> = OnceLock::new();
static STOP_GENERATION: AtomicU64 = AtomicU64::new(0);
pub static SHUTTING_DOWN: AtomicBool = AtomicBool::new(false);

fn write_serializable<T: Serialize>(value: &T) -> io::Result<()> {
    let data = serde_json::to_vec(value).map_err(io::Error::other)?;
    let mut output = OUTPUT
        .get_or_init(|| Mutex::new(io::stdout()))
        .lock()
        .map_err(|_| io::Error::other("output lock poisoned"))?;
    output.write_all(&(data.len() as u32).to_le_bytes())?;
    output.write_all(&data)?;
    output.flush()
}
fn emit_event(value: Value) {
    let _ = write_serializable(&value);
}
fn cancel_active(request: Option<u64>) -> bool {
    let mut matched = request.is_none();
    if let Ok(active) = ACTIVE.get_or_init(|| Mutex::new(None)).lock()
        && let Some((id, cancel)) = &*active
        && (request.is_none() || request == Some(*id))
    {
        cancel.store(true, Ordering::SeqCst);
        matched = true;
    }
    !matched || input::release_all()
}
fn local_stop() {
    cancel_active(None);
    STOP_GENERATION.fetch_add(1, Ordering::SeqCst);
    overlay::hide_global();
    emit_event(json!({"event":"local_stop"}));
}

fn main() -> io::Result<()> {
    unsafe {
        let _ = SetProcessDpiAwarenessContext(DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2);
    }
    let (sender, receiver) = mpsc::sync_channel::<(Request, Arc<AtomicBool>)>(1);
    std::thread::spawn(move || {
        let mut input = io::stdin().lock();
        while let Ok(Some(request)) = read_request(&mut input) {
            if request.method == "cancel" || request.method == "stop" {
                let target = if request.method == "cancel" {
                    request.params["request_id"].as_u64()
                } else {
                    None
                };
                let cleaned = cancel_active(target);
                if request.method == "stop" {
                    STOP_GENERATION.fetch_add(1, Ordering::SeqCst);
                    overlay::hide_global();
                }
                if !cleaned {
                    emit_event(json!({"id":request.id,"error":"native input cleanup incomplete"}));
                } else if request.method == "stop" && uia::semantic_settling() {
                    emit_event(
                        json!({"id":request.id,"error":"native semantic action still settling"}),
                    );
                } else {
                    emit_event(json!({"id":request.id,"result":{"success":true}}));
                }
                continue;
            }
            let cancel = Arc::new(AtomicBool::new(false));
            *ACTIVE.get_or_init(|| Mutex::new(None)).lock().unwrap() =
                Some((request.id, cancel.clone()));
            if sender.send((request, cancel)).is_err() {
                break;
            }
        }
        local_stop();
        SHUTTING_DOWN.store(true, Ordering::SeqCst);
        overlay::shutdown_global();
    });
    let mut engine = Engine::new();
    while let Ok((request, cancel)) = receiver.recv() {
        input::set_cancel(cancel);
        let executed = check_cancel().and_then(|_| engine.execute(&request));
        if executed.is_err() {
            input::release_all();
        }
        let response = match executed {
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
        write_serializable(&response)?;
        let mut active = ACTIVE.get_or_init(|| Mutex::new(None)).lock().unwrap();
        if active.as_ref().is_some_and(|(id, _)| *id == request.id) {
            *active = None;
        }
    }
    input::release_all();
    SHUTTING_DOWN.store(true, Ordering::SeqCst);
    overlay::shutdown_global();
    Ok(())
}

/// What the pointer shows now, among the system's own cursors, so a test can see an app change it (a move cursor over
/// something draggable, a hand over a link); "other" for an app's own cursor, "hidden" when none shows.
fn cursor_shape() -> &'static str {
    use windows::Win32::UI::WindowsAndMessaging::{
        CURSOR_SHOWING, CURSORINFO, GetCursorInfo, IDC_APPSTARTING, IDC_ARROW, IDC_CROSS, IDC_HAND,
        IDC_IBEAM, IDC_NO, IDC_SIZEALL, IDC_SIZENESW, IDC_SIZENS, IDC_SIZENWSE, IDC_SIZEWE,
        IDC_WAIT, LoadCursorW,
    };
    let mut info = CURSORINFO {
        cbSize: std::mem::size_of::<CURSORINFO>() as u32,
        ..Default::default()
    };
    if unsafe { GetCursorInfo(&mut info) }.is_err() {
        return "unknown";
    }
    if info.flags.0 & CURSOR_SHOWING.0 == 0 {
        return "hidden";
    }
    let shapes = [
        (IDC_ARROW, "arrow"),
        (IDC_SIZEALL, "move"),
        (IDC_HAND, "hand"),
        (IDC_IBEAM, "text"),
        (IDC_SIZEWE, "resize_horizontal"),
        (IDC_SIZENS, "resize_vertical"),
        (IDC_SIZENWSE, "resize_diagonal_down"),
        (IDC_SIZENESW, "resize_diagonal_up"),
        (IDC_WAIT, "wait"),
        (IDC_APPSTARTING, "busy"),
        (IDC_NO, "not_allowed"),
        (IDC_CROSS, "crosshair"),
    ];
    for (id, name) in shapes {
        if unsafe { LoadCursorW(None, id) }.is_ok_and(|system| system == info.hCursor) {
            return name;
        }
    }
    "other"
}
