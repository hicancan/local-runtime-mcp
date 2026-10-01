use super::{Element, Rectangle, error_string, interactive_control, rectangle, role_name};
use std::collections::HashMap;
use std::sync::{
    Arc, Mutex,
    atomic::{AtomicBool, Ordering},
    mpsc,
};
use std::time::{Duration, Instant};
use windows::Win32::Foundation::HWND;
use windows::Win32::System::Com::{
    CLSCTX_INPROC_SERVER, COINIT_MULTITHREADED, CoCreateInstance, CoInitializeEx, CoUninitialize,
};
use windows::Win32::System::Ole::{
    SafeArrayDestroy, SafeArrayGetElement, SafeArrayGetLBound, SafeArrayGetUBound,
};
use windows::Win32::UI::Accessibility::{
    CUIAutomation, IUIAutomation, IUIAutomationElement, IUIAutomationValuePattern,
    UIA_ValuePatternId,
};

enum Command {
    Collect(
        isize,
        Rectangle,
        usize,
        bool,
        mpsc::Sender<Result<(Vec<Element>, bool), String>>,
    ),
    Resolve(u64, mpsc::Sender<Result<Rectangle, String>>),
    SetValue(u64, String, isize, mpsc::Sender<Result<(), String>>),
}
struct Work {
    command: Command,
    deadline: Instant,
    cancel: Arc<AtomicBool>,
    stop_generation: u64,
}
static SEMANTIC_ACTIVE: Mutex<bool> = Mutex::new(false);
pub fn semantic_settling() -> bool {
    SEMANTIC_ACTIVE.lock().map(|value| *value).unwrap_or(true)
}
pub struct Worker {
    sender: mpsc::SyncSender<Work>,
    busy: Arc<AtomicBool>,
}
struct LiveElement {
    element: IUIAutomationElement,
    runtime_id: Vec<i32>,
}
impl Worker {
    pub fn new() -> Self {
        let (sender, receiver) = mpsc::sync_channel::<Work>(1);
        let busy = Arc::new(AtomicBool::new(false));
        let thread_busy = busy.clone();
        std::thread::spawn(move || unsafe {
            let initialized = CoInitializeEx(None, COINIT_MULTITHREADED).is_ok();
            let automation: Result<IUIAutomation, _> =
                CoCreateInstance(&CUIAutomation, None, CLSCTX_INPROC_SERVER);
            let mut refs = HashMap::<u64, LiveElement>::new();
            let mut sequence = 0u64;
            while let Ok(work) = receiver.recv() {
                let available =
                    Instant::now() < work.deadline && !work.cancel.load(Ordering::SeqCst);
                match work.command {
                    Command::Collect(hwnd, origin, limit, retain, response) => {
                        let result = if !available {
                            Err("accessibility operation cancelled".into())
                        } else if let Ok(automation) = &automation {
                            collect(
                                automation,
                                HWND(hwnd as *mut _),
                                origin,
                                (limit, retain),
                                &mut refs,
                                &mut sequence,
                                &work.cancel,
                            )
                        } else {
                            Err("UI Automation unavailable".into())
                        };
                        let _ = response.send(result);
                    }
                    Command::Resolve(key, response) => {
                        let result = if !available {
                            Err("accessibility operation cancelled".into())
                        } else {
                            resolve(refs.get(&key))
                        };
                        let _ = response.send(result);
                    }
                    Command::SetValue(key, value, foreground, response) => {
                        let result = (|| {
                            if !available {
                                return Err("accessibility operation cancelled".into());
                            }
                            let element =
                                refs.get(&key).ok_or("element_ref expired; observe again")?;
                            let _ = resolve(Some(element))?;
                            let pattern: IUIAutomationValuePattern = element
                                .element
                                .GetCurrentPatternAs(UIA_ValuePatternId)
                                .map_err(
                                    |_| "set_value unsupported: element has no UIA ValuePattern",
                                )?;
                            if pattern.CurrentIsReadOnly().map_err(error_string)?.as_bool() {
                                return Err("set_value unsupported: value is read-only".into());
                            }
                            if work.cancel.load(Ordering::SeqCst) {
                                return Err("set_value cancelled before dispatch".into());
                            }
                            if windows::Win32::UI::WindowsAndMessaging::GetForegroundWindow().0
                                as isize
                                != foreground
                            {
                                return Err(
                                    "foreground changed before semantic action; observe again"
                                        .into(),
                                );
                            }
                            {
                                let mut active = SEMANTIC_ACTIVE.lock().map_err(error_string)?;
                                if work.stop_generation
                                    != super::STOP_GENERATION.load(Ordering::SeqCst)
                                    || work.cancel.load(Ordering::SeqCst)
                                {
                                    return Err("set_value cancelled before dispatch".into());
                                }
                                *active = true;
                            }
                            let result = pattern
                                .SetValue(&windows::core::BSTR::from(value.as_str()))
                                .map_err(error_string);
                            *SEMANTIC_ACTIVE.lock().map_err(error_string)? = false;
                            result
                        })();
                        let _ = response.send(result);
                    }
                }
                thread_busy.store(false, Ordering::SeqCst);
            }
            if initialized {
                CoUninitialize()
            }
        });
        Self { sender, busy }
    }
    fn submit(&self, command: Command) -> Result<Arc<AtomicBool>, String> {
        if self
            .busy
            .compare_exchange(false, true, Ordering::SeqCst, Ordering::SeqCst)
            .is_err()
        {
            return Err("UI Automation busy or previous provider timed out".into());
        }
        let cancel = Arc::new(AtomicBool::new(false));
        let work = Work {
            command,
            deadline: Instant::now() + Duration::from_secs(2),
            cancel: cancel.clone(),
            stop_generation: super::STOP_GENERATION.load(Ordering::SeqCst),
        };
        if self.sender.try_send(work).is_err() {
            self.busy.store(false, Ordering::SeqCst);
            return Err("UI Automation unavailable".into());
        }
        Ok(cancel)
    }
    fn receive<T>(
        &self,
        receiver: mpsc::Receiver<Result<T, String>>,
        cancel: Arc<AtomicBool>,
    ) -> Result<T, String> {
        for _ in 0..100 {
            if super::input::check_cancel().is_err() {
                cancel.store(true, Ordering::SeqCst);
                return Err("accessibility operation cancelled".into());
            }
            match receiver.recv_timeout(Duration::from_millis(20)) {
                Ok(result) => return result,
                Err(mpsc::RecvTimeoutError::Disconnected) => {
                    return Err("UI Automation unavailable".into());
                }
                Err(_) => {}
            }
        }
        cancel.store(true, Ordering::SeqCst);
        Err("UI Automation timed out; outcome may be unknown for submitted semantic actions".into())
    }
    pub fn collect(
        &self,
        window: HWND,
        origin: Rectangle,
        limit: usize,
        retain: bool,
    ) -> (Vec<Element>, bool, String) {
        let (sender, receiver) = mpsc::channel();
        let result = self
            .submit(Command::Collect(
                window.0 as isize,
                origin,
                limit,
                retain,
                sender,
            ))
            .and_then(|cancel| self.receive(receiver, cancel));
        match result {
            Ok((elements, truncated)) => (
                elements,
                truncated,
                if truncated { "truncated" } else { "available" }.into(),
            ),
            Err(error) => (Vec::new(), false, error),
        }
    }
    pub fn resolve(&self, key: u64) -> Result<Rectangle, String> {
        let (sender, receiver) = mpsc::channel();
        let cancel = self.submit(Command::Resolve(key, sender))?;
        self.receive(receiver, cancel)
    }
    pub fn set_value(&self, key: u64, value: String, foreground: isize) -> Result<(), String> {
        let (sender, receiver) = mpsc::channel();
        let cancel = self.submit(Command::SetValue(key, value, foreground, sender))?;
        self.receive(receiver, cancel)
    }
}
unsafe fn resolve(live: Option<&LiveElement>) -> Result<Rectangle, String> {
    let live = live.ok_or("element_ref expired; observe again")?;
    let element = &live.element;
    if unsafe { runtime_id(element) }? != live.runtime_id {
        return Err("UIA element identity changed; observe again".into());
    }
    if !unsafe { element.CurrentIsEnabled() }
        .map_err(error_string)?
        .as_bool()
    {
        return Err("UIA element disabled".into());
    }
    let rect = rectangle(
        unsafe { element.CurrentBoundingRectangle() }
            .map_err(|_| "UIA element no longer available; observe again")?,
    );
    if rect.width <= 0 || rect.height <= 0 {
        return Err("UIA element has no current on-screen bounds".into());
    }
    Ok(rect)
}
unsafe fn collect(
    automation: &IUIAutomation,
    window: HWND,
    origin: Rectangle,
    options: (usize, bool),
    refs: &mut HashMap<u64, LiveElement>,
    sequence: &mut u64,
    cancel: &AtomicBool,
) -> Result<(Vec<Element>, bool), String> {
    let (limit, retain) = options;
    let root = unsafe { automation.ElementFromHandle(window) }.map_err(error_string)?;
    let walker = unsafe { automation.ControlViewWalker() }.map_err(error_string)?;
    let mut stack = Vec::new();
    if let Ok(first) = unsafe { walker.GetFirstChildElement(&root) } {
        stack.push(first)
    }
    let mut elements = Vec::new();
    let mut visited = 0;
    if retain && refs.len() + limit > 32_000 {
        refs.clear()
    }
    while let Some(element) = stack.pop() {
        if cancel.load(Ordering::SeqCst) {
            return Err("accessibility observation cancelled".into());
        }
        visited += 1;
        if visited > 5_000 {
            return Ok((elements, true));
        }
        if let Ok(sibling) = unsafe { walker.GetNextSiblingElement(&element) } {
            stack.push(sibling)
        }
        if let Ok(child) = unsafe { walker.GetFirstChildElement(&element) } {
            stack.push(child)
        }
        let kind = match unsafe { element.CurrentControlType() } {
            Ok(value) => value.0,
            Err(_) => continue,
        };
        if !interactive_control(kind) {
            continue;
        }
        let rect = match unsafe { element.CurrentBoundingRectangle() } {
            Ok(value) => rectangle(value),
            Err(_) => continue,
        };
        if rect.width <= 0 || rect.height <= 0 {
            continue;
        }
        let key = if retain {
            *sequence += 1;
            *sequence
        } else {
            0
        };
        elements.push(Element {
            ref_id: String::new(),
            role: role_name(kind).into(),
            name: unsafe { element.CurrentName() }
                .map(|value| value.to_string())
                .unwrap_or_default(),
            bounds: Rectangle {
                x: rect.x - origin.x,
                y: rect.y - origin.y,
                width: rect.width,
                height: rect.height,
            },
            disabled: unsafe { element.CurrentIsEnabled() }
                .map(|value| !value.as_bool())
                .unwrap_or(false),
            native_ref: key,
        });
        if retain && let Ok(runtime_id) = unsafe { runtime_id(&element) } {
            refs.insert(
                key,
                LiveElement {
                    element,
                    runtime_id,
                },
            );
        }
        if elements.len() == limit {
            return Ok((elements, !stack.is_empty()));
        }
    }
    Ok((elements, false))
}

unsafe fn runtime_id(element: &IUIAutomationElement) -> Result<Vec<i32>, String> {
    let array = unsafe { element.GetRuntimeId() }.map_err(error_string)?;
    if array.is_null() {
        return Err("UIA element has no runtime identity".into());
    }
    let result = (|| {
        let lower = unsafe { SafeArrayGetLBound(array, 1) }.map_err(error_string)?;
        let upper = unsafe { SafeArrayGetUBound(array, 1) }.map_err(error_string)?;
        if upper < lower || upper - lower > 128 {
            return Err("UIA runtime identity has invalid dimensions".into());
        }
        let mut output = Vec::new();
        for index in lower..=upper {
            let mut value = 0i32;
            unsafe { SafeArrayGetElement(array, &index, &mut value as *mut i32 as *mut _) }
                .map_err(error_string)?;
            output.push(value);
        }
        Ok(output)
    })();
    let _ = unsafe { SafeArrayDestroy(array) };
    result
}
