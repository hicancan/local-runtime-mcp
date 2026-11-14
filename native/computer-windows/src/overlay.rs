//! The only visible trace of physical control: a small translucent pill at the bottom of the
//! display being worked on, shown while a workflow holds the pointer and keyboard. Clicking it
//! stops that workflow. It never outlines windows, never takes focus and is excluded from capture.
use super::Rectangle;
use std::sync::{OnceLock, mpsc};
use std::time::Duration;
use windows::Win32::Foundation::{COLORREF, HWND, LPARAM, LRESULT, POINT, RECT, WPARAM};
use windows::Win32::Graphics::Dwm::{
    DWM_WINDOW_CORNER_PREFERENCE, DWMWA_WINDOW_CORNER_PREFERENCE, DWMWCP_ROUND,
    DwmSetWindowAttribute,
};
use windows::Win32::Graphics::Gdi::*;
use windows::Win32::System::LibraryLoader::GetModuleHandleW;
use windows::Win32::UI::HiDpi::{GetDpiForMonitor, MDT_EFFECTIVE_DPI};
use windows::Win32::UI::WindowsAndMessaging::*;
use windows::core::w;

const LABEL: &str = "AI 正在操作 · 点此停止";

enum Command {
    Show(u64),
    Near(Rectangle),
    Hide,
}
static CHANNEL: OnceLock<mpsc::Sender<Command>> = OnceLock::new();

pub struct Overlay {
    sender: mpsc::Sender<Command>,
    ready: bool,
}

impl Overlay {
    pub fn new() -> Self {
        let (sender, receiver) = mpsc::channel();
        let (ready_sender, ready_receiver) = mpsc::sync_channel(1);
        let _ = CHANNEL.set(sender.clone());
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
                w!("AI control"),
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
            let _ = ready_sender.send(true);
            let mut active = false;
            let mut near: Option<Rectangle> = None;
            loop {
                while let Ok(command) = receiver.try_recv() {
                    match command {
                        Command::Show(generation) => {
                            if generation
                                == super::STOP_GENERATION.load(std::sync::atomic::Ordering::SeqCst)
                            {
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
                let mut message = MSG::default();
                while PeekMessageW(&mut message, None, 0, 0, PM_REMOVE).as_bool() {
                    let _ = TranslateMessage(&message);
                    DispatchMessageW(&message);
                }
                if super::SHUTTING_DOWN.load(std::sync::atomic::Ordering::SeqCst) {
                    let _ = DestroyWindow(pill);
                    return;
                }
                std::thread::sleep(Duration::from_millis(8));
            }
        });
        Self {
            sender,
            ready: ready_receiver
                .recv_timeout(Duration::from_secs(2))
                .unwrap_or(false),
        }
    }

    pub fn show(&self) -> Result<(), String> {
        let generation = super::STOP_GENERATION.load(std::sync::atomic::Ordering::SeqCst);
        super::input::check_cancel()?;
        if !self.ready {
            return Err(
                "desktop control indicator could not initialize with capture exclusion".into(),
            );
        }
        let _ = self.sender.send(Command::Show(generation));
        Ok(())
    }

    /// Moves the pill to the display holding the given bounds.
    pub fn near(&self, bounds: Rectangle) {
        let _ = self.sender.send(Command::Near(bounds));
    }
}

pub fn hide_global() {
    if let Some(sender) = CHANNEL.get() {
        let _ = sender.send(Command::Hide);
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
    let (width, height) = (scale(208), scale(32));
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
                let mut wide: Vec<u16> = LABEL.encode_utf16().collect();
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
