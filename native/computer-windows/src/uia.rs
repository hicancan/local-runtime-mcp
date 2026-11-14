//! UI Automation on one dedicated MTA thread: tree observation through a single cached
//! cross-process request, and semantic actions that operate a control through its
//! automation patterns without the foreground, the pointer or the keyboard.
use super::{Element, Rectangle, error_string, rectangle};
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
use windows::Win32::UI::Accessibility::*;

/// What an observation collects and which elements it reports.
#[derive(Clone, Default)]
pub struct Query {
    /// "all" reports every named, identified or operable element; "interactive" only operable ones.
    pub scope: String,
    pub name: String,
    pub automation_id: String,
    pub role: String,
    pub limit: usize,
    pub offset: usize,
}

enum Command {
    Collect(
        isize,
        Rectangle,
        Query,
        mpsc::Sender<Result<Collected, String>>,
    ),
    Semantic(u64, Semantic, mpsc::Sender<Result<(), String>>),
    Bounds(u64, mpsc::Sender<Result<Rectangle, String>>),
}

pub enum Semantic {
    Invoke,
    Toggle,
    Select,
    Expand,
    Collapse,
    SetValue(String),
    ScrollIntoView,
    ScrollBy(i32, i32),
}

pub struct Collected {
    pub elements: Vec<Element>,
    pub total: usize,
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

/// Bounded so a long session cannot grow the live element table without limit.
const MAX_LIVE: usize = 40_000;
/// Elements visited in one observation; deeper or wider trees report truncation.
const MAX_VISITED: usize = 20_000;

impl Worker {
    pub fn new() -> Self {
        let (sender, receiver) = mpsc::sync_channel::<Work>(1);
        let busy = Arc::new(AtomicBool::new(false));
        let thread_busy = busy.clone();
        std::thread::spawn(move || unsafe {
            let initialized = CoInitializeEx(None, COINIT_MULTITHREADED).is_ok();
            let automation: Result<IUIAutomation, _> =
                CoCreateInstance(&CUIAutomation, None, CLSCTX_INPROC_SERVER);
            let mut live = HashMap::<u64, LiveElement>::new();
            let mut sequence = 0u64;
            while let Ok(work) = receiver.recv() {
                let available =
                    Instant::now() < work.deadline && !work.cancel.load(Ordering::SeqCst);
                match work.command {
                    Command::Collect(hwnd, origin, query, response) => {
                        let result = if !available {
                            Err("accessibility operation cancelled".into())
                        } else if let Ok(automation) = &automation {
                            collect(
                                automation,
                                HWND(hwnd as *mut _),
                                origin,
                                &query,
                                &mut live,
                                &mut sequence,
                                &work.cancel,
                            )
                        } else {
                            Err("UI Automation unavailable".into())
                        };
                        let _ = response.send(result);
                    }
                    Command::Semantic(key, action, response) => {
                        let result = if !available {
                            Err("accessibility operation cancelled".into())
                        } else {
                            semantic(live.get(&key), action, work.stop_generation, &work.cancel)
                        };
                        let _ = response.send(result);
                    }
                    Command::Bounds(key, response) => {
                        let result = if !available {
                            Err("accessibility operation cancelled".into())
                        } else {
                            current_bounds(live.get(&key))
                        };
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

    fn submit(&self, command: Command, budget: Duration) -> Result<Arc<AtomicBool>, String> {
        if self
            .busy
            .compare_exchange(false, true, Ordering::SeqCst, Ordering::SeqCst)
            .is_err()
        {
            return Err("UI Automation busy or a previous provider call timed out".into());
        }
        let cancel = Arc::new(AtomicBool::new(false));
        let work = Work {
            command,
            deadline: Instant::now() + budget,
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
        budget: Duration,
    ) -> Result<T, String> {
        let started = Instant::now();
        loop {
            if super::input::check_cancel().is_err() {
                cancel.store(true, Ordering::SeqCst);
                return Err("accessibility operation cancelled".into());
            }
            match receiver.recv_timeout(Duration::from_millis(20)) {
                Ok(result) => return result,
                Err(mpsc::RecvTimeoutError::Disconnected) => {
                    return Err("UI Automation unavailable".into());
                }
                Err(_) if started.elapsed() > budget => {
                    cancel.store(true, Ordering::SeqCst);
                    return Err(
                        "UI Automation timed out; a submitted semantic action may still take effect"
                            .into(),
                    );
                }
                Err(_) => {}
            }
        }
    }

    pub fn collect(
        &self,
        window: HWND,
        origin: Rectangle,
        query: Query,
    ) -> Result<Collected, String> {
        let budget = Duration::from_secs(8);
        let (sender, receiver) = mpsc::channel();
        let cancel = self.submit(
            Command::Collect(window.0 as isize, origin, query, sender),
            budget,
        )?;
        self.receive(receiver, cancel, budget)
    }

    /// Current on-screen bounds of a referenced element, for physical input aimed at it.
    pub fn bounds(&self, key: u64) -> Result<Rectangle, String> {
        let budget = Duration::from_secs(2);
        let (sender, receiver) = mpsc::channel();
        let cancel = self.submit(Command::Bounds(key, sender), budget)?;
        self.receive(receiver, cancel, budget)
    }

    pub fn semantic(&self, key: u64, action: Semantic) -> Result<(), String> {
        let budget = Duration::from_secs(5);
        let (sender, receiver) = mpsc::channel();
        let cancel = self.submit(Command::Semantic(key, action, sender), budget)?;
        self.receive(receiver, cancel, budget)
    }
}

const PATTERNS: [(UIA_PATTERN_ID, &str); 7] = [
    (UIA_InvokePatternId, "invoke"),
    (UIA_TogglePatternId, "toggle"),
    (UIA_SelectionItemPatternId, "select"),
    (UIA_ExpandCollapsePatternId, "expand_collapse"),
    (UIA_ValuePatternId, "value"),
    (UIA_ScrollItemPatternId, "scroll_into_view"),
    (UIA_ScrollPatternId, "scroll"),
];

unsafe fn cache_request(automation: &IUIAutomation) -> Result<IUIAutomationCacheRequest, String> {
    let request = unsafe { automation.CreateCacheRequest() }.map_err(error_string)?;
    unsafe {
        for property in [
            UIA_NamePropertyId,
            UIA_ControlTypePropertyId,
            UIA_AutomationIdPropertyId,
            UIA_ClassNamePropertyId,
            UIA_BoundingRectanglePropertyId,
            UIA_IsEnabledPropertyId,
            UIA_IsOffscreenPropertyId,
            UIA_HasKeyboardFocusPropertyId,
            UIA_RuntimeIdPropertyId,
            UIA_ToggleToggleStatePropertyId,
            UIA_ValueValuePropertyId,
            UIA_SelectionItemIsSelectedPropertyId,
            UIA_ExpandCollapseExpandCollapseStatePropertyId,
        ] {
            request.AddProperty(property).map_err(error_string)?;
        }
        for (pattern, _) in PATTERNS {
            request.AddPattern(pattern).map_err(error_string)?;
        }
        request
            .SetTreeScope(TreeScope_Subtree)
            .map_err(error_string)?;
        request
            .SetTreeFilter(&automation.ControlViewCondition().map_err(error_string)?)
            .map_err(error_string)?;
        request
            .SetAutomationElementMode(AutomationElementMode_Full)
            .map_err(error_string)?;
    }
    Ok(request)
}

fn text(value: windows::core::Result<windows::core::BSTR>) -> String {
    value
        .map(|value| value.to_string().trim().to_string())
        .unwrap_or_default()
}

unsafe fn collect(
    automation: &IUIAutomation,
    window: HWND,
    origin: Rectangle,
    query: &Query,
    live: &mut HashMap<u64, LiveElement>,
    sequence: &mut u64,
    cancel: &AtomicBool,
) -> Result<Collected, String> {
    let request = unsafe { cache_request(automation) }?;
    let root = unsafe { automation.ElementFromHandle(window) }.map_err(error_string)?;
    // One cross-process round trip brings the whole control-view subtree with its properties.
    let root = unsafe { root.BuildUpdatedCache(&request) }.map_err(error_string)?;
    if live.len() > MAX_LIVE {
        live.clear();
    }
    let interactive_only = query.scope == "interactive";
    let name_filter = query.name.to_lowercase();
    let mut elements = Vec::new();
    let mut matched = 0usize;
    let mut visited = 0usize;
    // Depth-first in document order: (element, depth, parent index in `elements`).
    let mut stack: Vec<(IUIAutomationElement, u32, Option<usize>)> = Vec::new();
    push_children(&root, 0, None, &mut stack);
    while let Some((element, depth, parent)) = stack.pop() {
        if cancel.load(Ordering::SeqCst) {
            return Err("accessibility observation cancelled".into());
        }
        visited += 1;
        if visited > MAX_VISITED {
            return Err(format!(
                "the window exposes more than {MAX_VISITED} automation elements; narrow the query"
            ));
        }
        let kind = unsafe { element.CachedControlType() }
            .map(|value| value.0)
            .unwrap_or(0);
        let name = text(unsafe { element.CachedName() });
        let automation_id = text(unsafe { element.CachedAutomationId() });
        let patterns: Vec<&'static str> = PATTERNS
            .iter()
            .filter(|(pattern, _)| {
                unsafe { element.GetCachedPatternAs::<windows::core::IUnknown>(*pattern) }.is_ok()
            })
            .map(|(_, label)| *label)
            .collect();
        let role = role_name(kind);
        // Every element in a scrollable view can scroll itself into view; that alone is not operable.
        let operable = patterns
            .iter()
            .any(|pattern| *pattern != "scroll_into_view")
            || interactive_role(kind);
        let reported = if interactive_only {
            operable
        } else {
            operable || !name.is_empty() || !automation_id.is_empty()
        };
        let mut index = parent;
        if reported {
            let matches = (name_filter.is_empty() || name.to_lowercase().contains(&name_filter))
                && (query.automation_id.is_empty() || automation_id == query.automation_id)
                && (query.role.is_empty() || role == query.role);
            if matches {
                matched += 1;
                if matched > query.offset && elements.len() < query.limit {
                    let rect = unsafe { element.CachedBoundingRectangle() }
                        .map(rectangle)
                        .unwrap_or_default();
                    *sequence += 1;
                    let key = *sequence;
                    if let Ok(runtime_id) = unsafe { cached_runtime_id(&element) } {
                        live.insert(
                            key,
                            LiveElement {
                                element: element.clone(),
                                runtime_id,
                            },
                        );
                    }
                    elements.push(Element {
                        ref_id: String::new(),
                        role: role.into(),
                        name,
                        automation_id,
                        class_name: text(unsafe { element.CachedClassName() }),
                        value: unsafe { value_of(&element) },
                        bounds: if rect.width > 0 && rect.height > 0 {
                            Some(Rectangle {
                                x: rect.x - origin.x,
                                y: rect.y - origin.y,
                                width: rect.width,
                                height: rect.height,
                            })
                        } else {
                            None
                        },
                        depth,
                        parent: parent.map(|value| value as u32),
                        disabled: !unsafe { element.CachedIsEnabled() }
                            .map(|value| value.as_bool())
                            .unwrap_or(true),
                        offscreen: unsafe { element.CachedIsOffscreen() }
                            .map(|value| value.as_bool())
                            .unwrap_or(false),
                        focused: unsafe { element.CachedHasKeyboardFocus() }
                            .map(|value| value.as_bool())
                            .unwrap_or(false),
                        patterns,
                        native_ref: key,
                    });
                    index = Some(elements.len() - 1);
                }
            }
        }
        push_children(&element, depth + 1, index, &mut stack);
    }
    Ok(Collected {
        elements,
        total: matched,
    })
}

fn push_children(
    element: &IUIAutomationElement,
    depth: u32,
    parent: Option<usize>,
    stack: &mut Vec<(IUIAutomationElement, u32, Option<usize>)>,
) {
    let Ok(children) = (unsafe { element.GetCachedChildren() }) else {
        return;
    };
    let count = unsafe { children.Length() }.unwrap_or(0);
    // Reverse push keeps document order when popping.
    for index in (0..count).rev() {
        if let Ok(child) = unsafe { children.GetElement(index) } {
            stack.push((child, depth, parent));
        }
    }
}

/// The current state a reader needs to judge a control: its text value, toggle, selection or expansion.
unsafe fn value_of(element: &IUIAutomationElement) -> Option<String> {
    unsafe {
        if let Ok(pattern) =
            element.GetCachedPatternAs::<IUIAutomationTogglePattern>(UIA_TogglePatternId)
        {
            return pattern.CachedToggleState().ok().map(|state| {
                if state == ToggleState_On {
                    "on"
                } else if state == ToggleState_Off {
                    "off"
                } else {
                    "mixed"
                }
                .into()
            });
        }
        if let Ok(pattern) = element
            .GetCachedPatternAs::<IUIAutomationSelectionItemPattern>(UIA_SelectionItemPatternId)
            && pattern
                .CachedIsSelected()
                .map(|value| value.as_bool())
                .unwrap_or(false)
        {
            return Some("selected".into());
        }
        if let Ok(pattern) =
            element.GetCachedPatternAs::<IUIAutomationValuePattern>(UIA_ValuePatternId)
            && let Ok(value) = pattern.CachedValue()
        {
            let value = value.to_string();
            if !value.is_empty() {
                return Some(value.chars().take(2000).collect());
            }
        }
        if let Ok(pattern) = element
            .GetCachedPatternAs::<IUIAutomationExpandCollapsePattern>(UIA_ExpandCollapsePatternId)
            && let Ok(state) = pattern.CachedExpandCollapseState()
        {
            return Some(
                if state == ExpandCollapseState_Expanded {
                    "expanded"
                } else if state == ExpandCollapseState_Collapsed {
                    "collapsed"
                } else {
                    "partial"
                }
                .into(),
            );
        }
    }
    None
}

unsafe fn current_bounds(live: Option<&LiveElement>) -> Result<Rectangle, String> {
    let live = live.ok_or("element_ref expired; observe again")?;
    if unsafe { current_runtime_id(&live.element) }? != live.runtime_id {
        return Err("UIA element identity changed; observe again".into());
    }
    let rect = rectangle(
        unsafe { live.element.CurrentBoundingRectangle() }
            .map_err(|_| "UIA element no longer available; observe again")?,
    );
    if rect.width <= 0 || rect.height <= 0 {
        return Err("UIA element has no current on-screen bounds".into());
    }
    Ok(rect)
}

unsafe fn semantic(
    live: Option<&LiveElement>,
    action: Semantic,
    stop_generation: u64,
    cancel: &AtomicBool,
) -> Result<(), String> {
    let live = live.ok_or("element_ref expired; observe again")?;
    let element = &live.element;
    if unsafe { current_runtime_id(element) }? != live.runtime_id {
        return Err("UIA element identity changed; observe again".into());
    }
    if !unsafe { element.CurrentIsEnabled() }
        .map_err(|_| "UIA element no longer available; observe again")?
        .as_bool()
    {
        return Err("UIA element is disabled".into());
    }
    fn missing(label: &str) -> String {
        format!("element does not support the {label} pattern; see its patterns list")
    }
    {
        let mut active = SEMANTIC_ACTIVE.lock().map_err(error_string)?;
        if stop_generation != super::STOP_GENERATION.load(Ordering::SeqCst)
            || cancel.load(Ordering::SeqCst)
        {
            return Err("semantic action cancelled before dispatch".into());
        }
        *active = true;
    }
    let result = unsafe {
        match action {
            Semantic::Invoke => element
                .GetCurrentPatternAs::<IUIAutomationInvokePattern>(UIA_InvokePatternId)
                .map_err(|_| missing("invoke"))?
                .Invoke()
                .map_err(error_string),
            Semantic::Toggle => element
                .GetCurrentPatternAs::<IUIAutomationTogglePattern>(UIA_TogglePatternId)
                .map_err(|_| missing("toggle"))?
                .Toggle()
                .map_err(error_string),
            Semantic::Select => element
                .GetCurrentPatternAs::<IUIAutomationSelectionItemPattern>(
                    UIA_SelectionItemPatternId,
                )
                .map_err(|_| missing("select"))?
                .Select()
                .map_err(error_string),
            Semantic::Expand => element
                .GetCurrentPatternAs::<IUIAutomationExpandCollapsePattern>(
                    UIA_ExpandCollapsePatternId,
                )
                .map_err(|_| missing("expand_collapse"))?
                .Expand()
                .map_err(error_string),
            Semantic::Collapse => element
                .GetCurrentPatternAs::<IUIAutomationExpandCollapsePattern>(
                    UIA_ExpandCollapsePatternId,
                )
                .map_err(|_| missing("expand_collapse"))?
                .Collapse()
                .map_err(error_string),
            Semantic::SetValue(value) => {
                let pattern = element
                    .GetCurrentPatternAs::<IUIAutomationValuePattern>(UIA_ValuePatternId)
                    .map_err(|_| missing("value"))?;
                if pattern.CurrentIsReadOnly().map_err(error_string)?.as_bool() {
                    Err("value is read-only".into())
                } else {
                    pattern
                        .SetValue(&windows::core::BSTR::from(value.as_str()))
                        .map_err(error_string)
                }
            }
            Semantic::ScrollIntoView => element
                .GetCurrentPatternAs::<IUIAutomationScrollItemPattern>(UIA_ScrollItemPatternId)
                .map_err(|_| missing("scroll_into_view"))?
                .ScrollIntoView()
                .map_err(error_string),
            Semantic::ScrollBy(horizontal, vertical) => {
                let pattern = element
                    .GetCurrentPatternAs::<IUIAutomationScrollPattern>(UIA_ScrollPatternId)
                    .map_err(|_| missing("scroll"))?;
                let step = |value: i32| match value.signum() {
                    1 => ScrollAmount_LargeIncrement,
                    -1 => ScrollAmount_LargeDecrement,
                    _ => ScrollAmount_NoAmount,
                };
                let mut result = Ok(());
                for _ in 0..horizontal
                    .unsigned_abs()
                    .max(vertical.unsigned_abs())
                    .min(50)
                {
                    result = pattern
                        .Scroll(step(horizontal), step(vertical))
                        .map_err(error_string);
                    if result.is_err() {
                        break;
                    }
                }
                result
            }
        }
    };
    *SEMANTIC_ACTIVE.lock().map_err(error_string)? = false;
    result
}

fn interactive_role(value: i32) -> bool {
    matches!(
        value,
        50000 | 50002 | 50003 | 50004 | 50005 | 50007 | 50011 | 50013 | 50015 | 50019 | 50024
    )
}

pub fn role_name(value: i32) -> &'static str {
    match value {
        50000 => "button",
        50001 => "calendar",
        50002 => "checkbox",
        50003 => "combobox",
        50004 => "textbox",
        50005 => "link",
        50006 => "image",
        50007 => "listitem",
        50008 => "list",
        50009 => "menu",
        50010 => "menubar",
        50011 => "menuitem",
        50012 => "progressbar",
        50013 => "radio",
        50014 => "scrollbar",
        50015 => "slider",
        50016 => "spinner",
        50017 => "statusbar",
        50018 => "tablist",
        50019 => "tab",
        50020 => "text",
        50021 => "toolbar",
        50022 => "tooltip",
        50023 => "tree",
        50024 => "treeitem",
        50025 => "custom",
        50026 => "group",
        50027 => "thumb",
        50028 => "datagrid",
        50029 => "dataitem",
        50030 => "document",
        50031 => "splitbutton",
        50032 => "window",
        50033 => "pane",
        50034 => "header",
        50035 => "headeritem",
        50036 => "table",
        50037 => "titlebar",
        50038 => "separator",
        50039 => "semanticzoom",
        50040 => "appbar",
        _ => "control",
    }
}

unsafe fn cached_runtime_id(element: &IUIAutomationElement) -> Result<Vec<i32>, String> {
    // The runtime ID is in the cache request; reading it current would cost a round trip per element.
    let mut value =
        unsafe { element.GetCachedPropertyValue(UIA_RuntimeIdPropertyId) }.map_err(error_string)?;
    let array = unsafe { value.Anonymous.Anonymous.Anonymous.parray };
    let result = unsafe { safe_array_ints(array) };
    let _ = unsafe { windows::Win32::System::Variant::VariantClear(&mut value) };
    result
}

unsafe fn current_runtime_id(element: &IUIAutomationElement) -> Result<Vec<i32>, String> {
    let array = unsafe { element.GetRuntimeId() }.map_err(error_string)?;
    let result = unsafe { safe_array_ints(array) };
    if !array.is_null() {
        let _ = unsafe { SafeArrayDestroy(array) };
    }
    result
}

unsafe fn safe_array_ints(
    array: *mut windows::Win32::System::Com::SAFEARRAY,
) -> Result<Vec<i32>, String> {
    if array.is_null() {
        return Err("UIA element has no runtime identity".into());
    }
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
}
