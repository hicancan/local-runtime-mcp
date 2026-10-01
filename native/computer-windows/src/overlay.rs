use super::Rectangle;
use std::sync::{OnceLock, mpsc};
use std::time::Duration;
use windows::Win32::Foundation::{COLORREF, HWND, LPARAM, LRESULT, RECT, WPARAM};
use windows::Win32::Graphics::Gdi::{
    BeginPaint, CreateSolidBrush, DeleteObject, EndPaint, FillRect, GetStockObject, NULL_BRUSH,
    PAINTSTRUCT, SetBkMode, SetTextColor, TRANSPARENT, TextOutW,
};
use windows::Win32::System::LibraryLoader::GetModuleHandleW;
use windows::Win32::UI::WindowsAndMessaging::*;
use windows::core::{PCWSTR, w};

enum Command {
    Show(String, Rectangle, u64),
    Target(Rectangle),
    Marker(i32, i32),
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
                hbrBackground: windows::Win32::Graphics::Gdi::HBRUSH(GetStockObject(NULL_BRUSH).0),
                ..Default::default()
            };
            RegisterClassW(&class);
            let mut windows = Vec::new();
            for index in 0..6 {
                let mut ex = WS_EX_TOPMOST | WS_EX_TOOLWINDOW | WS_EX_NOACTIVATE;
                if index != 4 {
                    ex |= WS_EX_TRANSPARENT;
                }
                let result = CreateWindowExW(
                    ex,
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
                    Some(index as *const _),
                );
                if let Ok(window) = result {
                    if SetWindowDisplayAffinity(window, WDA_EXCLUDEFROMCAPTURE).is_err() {
                        let _ = DestroyWindow(window);
                        let _ = ready_sender.send(false);
                        return;
                    }
                    windows.push(window)
                }
            }
            if windows.len() != 6 {
                let _ = ready_sender.send(false);
                return;
            }
            let _ = ready_sender.send(true);
            let mut active = false;
            loop {
                while let Ok(command) = receiver.try_recv() {
                    match command {
                        Command::Show(label, target, generation) => {
                            if generation
                                != super::STOP_GENERATION.load(std::sync::atomic::Ordering::SeqCst)
                            {
                                continue;
                            }
                            active = true;
                            let text = format!(
                                "AI · {}   |   STOP",
                                if label.is_empty() { "Desktop" } else { &label }
                            );
                            let wide: Vec<u16> =
                                text.encode_utf16().chain(std::iter::once(0)).collect();
                            let _ = SetWindowTextW(windows[4], PCWSTR(wide.as_ptr()));
                            place(&windows, target);
                        }
                        Command::Target(target) => {
                            if active {
                                place(&windows, target);
                            }
                        }
                        Command::Marker(x, y) => {
                            if active {
                                let _ = SetWindowPos(
                                    windows[5],
                                    Some(HWND_TOPMOST),
                                    x - 6,
                                    y - 6,
                                    13,
                                    13,
                                    SWP_NOACTIVATE | SWP_SHOWWINDOW,
                                );
                            }
                        }
                        Command::Hide => {
                            active = false;
                            for window in &windows {
                                let _ = ShowWindow(*window, SW_HIDE);
                            }
                        }
                    }
                }
                let mut message = MSG::default();
                while PeekMessageW(&mut message, None, 0, 0, PM_REMOVE).as_bool() {
                    let _ = TranslateMessage(&message);
                    DispatchMessageW(&message);
                }
                if super::SHUTTING_DOWN.load(std::sync::atomic::Ordering::SeqCst) {
                    for window in &windows {
                        let _ = DestroyWindow(*window);
                    }
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
    pub fn show(&self, label: &str, bounds: Rectangle) -> Result<(), String> {
        let generation = super::STOP_GENERATION.load(std::sync::atomic::Ordering::SeqCst);
        super::input::check_cancel()?;
        if !self.ready {
            return Err(
                "desktop control indicator could not initialize with capture exclusion".into(),
            );
        }
        let _ = self.sender.send(Command::Show(
            label.chars().take(60).collect(),
            bounds,
            generation,
        ));
        Ok(())
    }
    pub fn target(&self, bounds: Rectangle) {
        let _ = self.sender.send(Command::Target(bounds));
    }
    pub fn marker(&self, point: (i32, i32)) {
        let _ = self.sender.send(Command::Marker(point.0, point.1));
    }
}
pub fn hide_global() {
    if let Some(sender) = CHANNEL.get() {
        let _ = sender.send(Command::Hide);
    }
}
unsafe fn place(windows: &[HWND], bounds: Rectangle) {
    let positions = [
        (bounds.x, bounds.y, bounds.width, 3),
        (bounds.x, bounds.y + bounds.height - 3, bounds.width, 3),
        (bounds.x, bounds.y, 3, bounds.height),
        (bounds.x + bounds.width - 3, bounds.y, 3, bounds.height),
        (bounds.x + bounds.width - 320, bounds.y + 5, 315, 26),
    ];
    for (window, (x, y, width, height)) in windows.iter().zip(positions) {
        let _ = unsafe {
            SetWindowPos(
                *window,
                Some(HWND_TOPMOST),
                x,
                y,
                width,
                height,
                SWP_NOACTIVATE | SWP_SHOWWINDOW,
            )
        };
    }
}
unsafe extern "system" fn window_proc(
    hwnd: HWND,
    message: u32,
    wparam: WPARAM,
    lparam: LPARAM,
) -> LRESULT {
    match message {
        WM_NCCREATE => {
            let create = unsafe { &*(lparam.0 as *const CREATESTRUCTW) };
            unsafe { SetWindowLongPtrW(hwnd, GWLP_USERDATA, create.lpCreateParams as isize) };
            LRESULT(1)
        }
        WM_MOUSEACTIVATE => LRESULT(MA_NOACTIVATE as isize),
        WM_LBUTTONDOWN => {
            if unsafe { GetWindowLongPtrW(hwnd, GWLP_USERDATA) } == 4 {
                super::local_stop();
                hide_global();
            }
            LRESULT(0)
        }
        WM_NCHITTEST => {
            if unsafe { GetWindowLongPtrW(hwnd, GWLP_USERDATA) } != 4 {
                LRESULT(HTTRANSPARENT as isize)
            } else {
                LRESULT(HTCLIENT as isize)
            }
        }
        WM_PAINT => {
            let mut paint = PAINTSTRUCT::default();
            let dc = unsafe { BeginPaint(hwnd, &mut paint) };
            let brush = unsafe { CreateSolidBrush(COLORREF(0x00e89c24)) };
            let mut rect = RECT::default();
            let _ = unsafe { GetClientRect(hwnd, &mut rect) };
            unsafe {
                FillRect(dc, &rect, brush);
                let _ = DeleteObject(brush.into());
            }
            if unsafe { GetWindowLongPtrW(hwnd, GWLP_USERDATA) } == 4 {
                let mut text = [0u16; 180];
                let length = unsafe { GetWindowTextW(hwnd, &mut text) };
                unsafe {
                    SetBkMode(dc, TRANSPARENT);
                    SetTextColor(dc, COLORREF(0x00ffffff));
                    let _ = TextOutW(dc, 5, 5, &text[..length.max(0) as usize]);
                }
            }
            let _ = unsafe { EndPaint(hwnd, &paint) };
            LRESULT(0)
        }
        _ => unsafe { DefWindowProcW(hwnd, message, wparam, lparam) },
    }
}
