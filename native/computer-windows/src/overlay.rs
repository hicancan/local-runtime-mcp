//! The only visible trace of physical control: a small translucent pill at the bottom of the
//! display being worked on, shown while a workflow holds the pointer and keyboard. Clicking it
//! stops that workflow. It never outlines windows, never takes focus and is excluded from capture.
use super::Rectangle;
use std::sync::atomic::{AtomicU32, Ordering};
use std::sync::{Mutex, OnceLock, mpsc};
use std::time::Duration;
use windows::Win32::Foundation::{COLORREF, HWND, LPARAM, LRESULT, POINT, RECT, WPARAM};
use windows::Win32::Graphics::Dwm::{
    DWM_WINDOW_CORNER_PREFERENCE, DWMWA_WINDOW_CORNER_PREFERENCE, DWMWCP_ROUND,
    DwmSetWindowAttribute,
};
use windows::Win32::Graphics::Gdi::*;
use windows::Win32::System::LibraryLoader::GetModuleHandleW;
use windows::Win32::System::Threading::GetCurrentThreadId;
use windows::Win32::UI::HiDpi::{GetDpiForMonitor, MDT_EFFECTIVE_DPI};
use windows::Win32::UI::WindowsAndMessaging::*;
use windows::core::w;

const DEFAULT_LABEL: &str = "AI 正在操作 · 点此停止";
const WM_APP_CMD: u32 = WM_APP + 1;

enum Command {
    Show(u64, String),
    Near(Rectangle),
    Hide,
}

static CHANNEL: OnceLock<mpsc::Sender<Command>> = OnceLock::new();
static THREAD_ID: AtomicU32 = AtomicU32::new(0);
static ACTIVE_LABEL: OnceLock<Mutex<String>> = OnceLock::new();

fn wake_overlay_thread() {
    let tid = THREAD_ID.load(Ordering::SeqCst);
    if tid != 0 {
        unsafe {
            let _ = PostThreadMessageW(tid, WM_APP_CMD, WPARAM(0), LPARAM(0));
        }
    }
}

pub struct Overlay {
    sender: mpsc::Sender<Command>,
    ready: bool,
}

impl Overlay {
    pub fn new() -> Self {
        let (sender, receiver) = mpsc::channel();
        let (ready_sender, ready_receiver) = mpsc::sync_channel(1);
        let _ = CHANNEL.set(sender.clone());
        let _ = ACTIVE_LABEL.set(Mutex::new(DEFAULT_LABEL.to_string()));
        std::thread::spawn(move || unsafe {
            let instance = GetModuleHandleW(None).unwrap_or_default();
            let class = WNDCLASSW {
                lpfnWndProc: Some(window_proc),
                hInstance: instance.into(),
                lpszClassName: w!("LocalRuntimeMCPControl"),
                hCursor: LoadCursorW(None, IDC_HAND).unwrap_or_default(),
                ..Default::default()
            };
            RegisterClassW(&class);
            let Ok(pill) = CreateWindowExW(
                WS_EX_TOPMOST | WS_EX_TOOLWINDOW | WS_EX_NOACTIVATE | WS_EX_LAYERED,
                w!("LocalRuntimeMCPControl"),
                w!("Desktop control"),
                WS_POPUP,
                0,
                0,
                1,
                1,
                None,
                None,
                Some(instance.into()),
                None,
            ) else {
                let _ = ready_sender.send(false);
                return;
            };
            if SetWindowDisplayAffinity(pill, WDA_EXCLUDEFROMCAPTURE).is_err() {
                let _ = DestroyWindow(pill);
                let _ = ready_sender.send(false);
                return;
            }
            let _ = SetLayeredWindowAttributes(pill, COLORREF(0), 232, LWA_ALPHA);
            let corner: DWM_WINDOW_CORNER_PREFERENCE = DWMWCP_ROUND;
            let _ = DwmSetWindowAttribute(
                pill,
                DWMWA_WINDOW_CORNER_PREFERENCE,
                &corner as *const _ as *const _,
                size_of::<DWM_WINDOW_CORNER_PREFERENCE>() as u32,
            );
            // Force message queue creation before publishing thread ID so PostThreadMessageW never races.
            let mut init_msg = MSG::default();
            let _ = PeekMessageW(&mut init_msg, None, WM_USER, WM_USER, PM_NOREMOVE);
            THREAD_ID.store(GetCurrentThreadId(), Ordering::SeqCst);
            let _ = ready_sender.send(true);

            let mut active = false;
            let mut near: Option<Rectangle> = None;
            let mut message = MSG::default();
            // Pure event-driven Win32 message loop: 0% idle CPU and full C-state sleep.
            while GetMessageW(&mut message, None, 0, 0).as_bool() {
                if message.message == WM_APP_CMD {
                    while let Ok(command) = receiver.try_recv() {
                        match command {
                            Command::Show(generation, label) => {
                                if generation == super::STOP_GENERATION.load(Ordering::SeqCst) {
                                    if let Some(lock) = ACTIVE_LABEL.get()
                                        && let Ok(mut guard) = lock.lock()
                                    {
                                        *guard = format_pill_label(&label);
                                    }
                                    active = true;
                                    place(pill, near);
                                }
                            }
                            Command::Near(bounds) => {
                                near = Some(bounds);
                                if active {
                                    place(pill, near);
                                }
                            }
                            Command::Hide => {
                                active = false;
                                near = None;
                                let _ = ShowWindow(pill, SW_HIDE);
                            }
                        }
                    }
                    if super::SHUTTING_DOWN.load(Ordering::SeqCst) {
                        break;
                    }
                    continue;
                }
                let _ = TranslateMessage(&message);
                DispatchMessageW(&message);
                if super::SHUTTING_DOWN.load(Ordering::SeqCst) {
                    break;
                }
            }
            THREAD_ID.store(0, Ordering::SeqCst);
            let _ = DestroyWindow(pill);
        });
        Self {
            sender,
            ready: ready_receiver
                .recv_timeout(Duration::from_secs(2))
                .unwrap_or(false),
        }
    }

    pub fn show(&self, label: &str) -> Result<(), String> {
        let generation = super::STOP_GENERATION.load(Ordering::SeqCst);
        super::input::check_cancel()?;
        if !self.ready {
            return Err(
                "desktop control indicator could not initialize with capture exclusion".into(),
            );
        }
        let _ = self
            .sender
            .send(Command::Show(generation, label.trim().to_string()));
        wake_overlay_thread();
        Ok(())
    }

    /// Moves the pill to the display holding the given bounds.
    pub fn near(&self, bounds: Rectangle) {
        let _ = self.sender.send(Command::Near(bounds));
        wake_overlay_thread();
    }
}

fn format_pill_label(label: &str) -> String {
    let trimmed = label.trim();
    if trimmed.is_empty() {
        return DEFAULT_LABEL.to_string();
    }
    let clipped: String = trimmed.chars().take(24).collect();
    format!("{clipped} · 点此停止")
}

pub fn hide_global() {
    if let Some(sender) = CHANNEL.get() {
        let _ = sender.send(Command::Hide);
        wake_overlay_thread();
    }
}

pub fn shutdown_global() {
    let tid = THREAD_ID.load(Ordering::SeqCst);
    if tid != 0 {
        unsafe {
            let _ = PostThreadMessageW(tid, WM_QUIT, WPARAM(0), LPARAM(0));
        }
    }
}

unsafe fn place(pill: HWND, near: Option<Rectangle>) {
    let point = near
        .map(|bounds| POINT {
            x: bounds.x + bounds.width / 2,
            y: bounds.y + bounds.height / 2,
        })
        .unwrap_or_default();
    let monitor = unsafe { MonitorFromPoint(point, MONITOR_DEFAULTTOPRIMARY) };
    let mut info = MONITORINFO {
        cbSize: size_of::<MONITORINFO>() as u32,
        ..Default::default()
    };
    if !unsafe { GetMonitorInfoW(monitor, &mut info) }.as_bool() {
        return;
    }
    let (mut dpi, mut unused) = (96u32, 96u32);
    let _ = unsafe { GetDpiForMonitor(monitor, MDT_EFFECTIVE_DPI, &mut dpi, &mut unused) };
    let scale = |value: i32| value * dpi as i32 / 96;
    let char_count = ACTIVE_LABEL
        .get()
        .and_then(|lock| lock.lock().ok().map(|s| s.chars().count()))
        .unwrap_or(14);
    let base_w = (char_count as i32 * 13 + 40).clamp(208, 360);
    let (width, height) = (scale(base_w), scale(32));
    let work = info.rcWork;
    let x = (work.left + work.right - width) / 2;
    let y = work.bottom - height - scale(20);
    let _ = unsafe {
        SetWindowPos(
            pill,
            Some(HWND_TOPMOST),
            x,
            y,
            width,
            height,
            SWP_NOACTIVATE | SWP_SHOWWINDOW,
        )
    };
    let _ = unsafe { InvalidateRect(Some(pill), None, true) };
}

unsafe extern "system" fn window_proc(
    hwnd: HWND,
    message: u32,
    wparam: WPARAM,
    lparam: LPARAM,
) -> LRESULT {
    match message {
        WM_MOUSEACTIVATE => LRESULT(MA_NOACTIVATE as isize),
        WM_LBUTTONUP => {
            super::local_stop();
            hide_global();
            LRESULT(0)
        }
        WM_ERASEBKGND => LRESULT(1),
        WM_PAINT => {
            let mut paint = PAINTSTRUCT::default();
            let dc = unsafe { BeginPaint(hwnd, &mut paint) };
            let mut rect = RECT::default();
            let _ = unsafe { GetClientRect(hwnd, &mut rect) };
            let height = rect.bottom - rect.top;
            unsafe {
                let brush = CreateSolidBrush(COLORREF(0x00282320));
                FillRect(dc, &rect, brush);
                let _ = DeleteObject(brush.into());
                let font = CreateFontW(
                    -(height * 13 / 32),
                    0,
                    0,
                    0,
                    FW_MEDIUM.0 as i32,
                    0,
                    0,
                    0,
                    DEFAULT_CHARSET,
                    OUT_DEFAULT_PRECIS,
                    CLIP_DEFAULT_PRECIS,
                    CLEARTYPE_QUALITY,
                    0,
                    w!("Microsoft YaHei UI"),
                );
                let previous = SelectObject(dc, font.into());
                SetBkMode(dc, TRANSPARENT);
                SetTextColor(dc, COLORREF(0x00F2F2F2));
                let label_text = ACTIVE_LABEL
                    .get()
                    .and_then(|lock| lock.lock().ok().map(|s| s.clone()))
                    .unwrap_or_else(|| DEFAULT_LABEL.to_string());
                let mut wide: Vec<u16> = label_text.encode_utf16().collect();
                let _ = DrawTextW(
                    dc,
                    &mut wide,
                    &mut rect,
                    DT_CENTER | DT_VCENTER | DT_SINGLELINE,
                );
                SelectObject(dc, previous);
                let _ = DeleteObject(font.into());
            }
            let _ = unsafe { EndPaint(hwnd, &paint) };
            LRESULT(0)
        }
        _ => unsafe { DefWindowProcW(hwnd, message, wparam, lparam) },
    }
}
