use serde::Serialize;

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub(crate) struct SurfaceBounds {
    pub x: i32,
    pub y: i32,
    pub width: i32,
    pub height: i32,
}

#[derive(Clone, Copy, Debug)]
pub(crate) struct VideoSurface {
    parent_hwnd: isize,
    hwnd: isize,
    bounds: SurfaceBounds,
    visible: bool,
}

impl VideoSurface {
    pub(crate) fn parent_hwnd(self) -> isize {
        self.parent_hwnd
    }

    pub(crate) fn hwnd(self) -> isize {
        self.hwnd
    }

    pub(crate) fn bounds(self) -> SurfaceBounds {
        self.bounds
    }

    pub(crate) fn visible(self) -> bool {
        self.visible
    }

    pub(crate) fn is_valid(self) -> bool {
        platform::is_valid(self)
    }

    pub(crate) fn set_bounds(
        &mut self,
        bounds: SurfaceBounds,
        visible: bool,
    ) -> Result<bool, SurfaceError> {
        let (next, changed) = platform::set_bounds(self, bounds, visible)?;
        self.bounds = next;
        self.visible = visible && next.width > 0 && next.height > 0;
        Ok(changed)
    }
}

#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub(crate) struct NativeWindowMetrics {
    pub client_width: i32,
    pub client_height: i32,
    pub window_left: i32,
    pub window_top: i32,
    pub window_right: i32,
    pub window_bottom: i32,
    pub dpi: u32,
    pub dpi_scale: f64,
    pub monitor_left: i32,
    pub monitor_top: i32,
    pub monitor_right: i32,
    pub monitor_bottom: i32,
    pub work_left: i32,
    pub work_top: i32,
    pub work_right: i32,
    pub work_bottom: i32,
    pub mpv_child: Option<ChildWindowMetrics>,
}

#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub(crate) struct ChildWindowMetrics {
    pub class_name: String,
    pub owner_pid: u32,
    pub parent_matches: bool,
    pub style: u32,
    pub visible: bool,
    pub width: i32,
    pub height: i32,
    pub client_width: i32,
    pub client_height: i32,
}

#[derive(Clone, Copy, Debug)]
pub(crate) struct SurfaceError;

impl std::fmt::Display for SurfaceError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter.write_str("native video surface operation failed")
    }
}

impl std::error::Error for SurfaceError {}

pub(crate) fn create(parent_hwnd: isize) -> Result<VideoSurface, SurfaceError> {
    platform::create(parent_hwnd)
}

pub(crate) fn diagnostics(
    parent_hwnd: isize,
    surface: Option<VideoSurface>,
) -> Result<NativeWindowMetrics, SurfaceError> {
    platform::diagnostics(parent_hwnd, surface)
}

#[cfg(windows)]
mod platform {
    use std::ptr;

    use windows_sys::Win32::Foundation::{HWND, RECT};
    use windows_sys::Win32::Graphics::Gdi::{
        GetMonitorInfoW, GetStockObject, MonitorFromWindow, BLACK_BRUSH, HMONITOR, MONITORINFO,
        MONITOR_DEFAULTTONEAREST,
    };
    use windows_sys::Win32::System::LibraryLoader::GetModuleHandleW;
    use windows_sys::Win32::System::Threading::GetCurrentProcessId;
    use windows_sys::Win32::UI::HiDpi::GetDpiForWindow;
    use windows_sys::Win32::UI::WindowsAndMessaging::{
        CreateWindowExW, DefWindowProcW, FindWindowExW, GetClassNameW, GetClientRect, GetParent,
        GetWindowLongPtrW, GetWindowRect, GetWindowThreadProcessId, IsWindow, IsWindowVisible,
        LoadCursorW, RegisterClassW, SetWindowPos, ShowWindow, CS_HREDRAW, CS_VREDRAW, GWL_STYLE,
        HWND_TOP, IDC_ARROW, SWP_NOACTIVATE, SWP_SHOWWINDOW, SW_HIDE, SW_SHOWNOACTIVATE, WNDCLASSW,
        WS_CHILD, WS_CLIPCHILDREN, WS_CLIPSIBLINGS,
    };

    use super::{
        ChildWindowMetrics, NativeWindowMetrics, SurfaceBounds, SurfaceError, VideoSurface,
    };

    const SURFACE_CLASS_NAME: &[u16] = &[
        b'D' as u16,
        b'o' as u16,
        b'r' as u16,
        b'i' as u16,
        b'y' as u16,
        b'a' as u16,
        b'W' as u16,
        b'0' as u16,
        b'M' as u16,
        b'p' as u16,
        b'v' as u16,
        b'S' as u16,
        b'u' as u16,
        b'r' as u16,
        b'f' as u16,
        b'a' as u16,
        b'c' as u16,
        b'e' as u16,
        0,
    ];

    fn hwnd(value: isize) -> HWND {
        value as HWND
    }

    fn client_size(parent: HWND) -> Result<(i32, i32), SurfaceError> {
        let mut client = RECT::default();
        // SAFETY: `parent` is a live Tauri window and `client` is writable.
        if unsafe { GetClientRect(parent, &mut client) } == 0 {
            return Err(SurfaceError);
        }
        Ok((
            (client.right - client.left).max(0),
            (client.bottom - client.top).max(0),
        ))
    }

    fn clamp_bounds(parent: HWND, bounds: SurfaceBounds) -> Result<SurfaceBounds, SurfaceError> {
        let (client_width, client_height) = client_size(parent)?;
        if bounds.width <= 0 || bounds.height <= 0 {
            return Ok(SurfaceBounds::default());
        }
        let x = bounds.x.clamp(0, client_width);
        let y = bounds.y.clamp(0, client_height);
        let width = bounds.width.min(client_width.saturating_sub(x));
        let height = bounds.height.min(client_height.saturating_sub(y));
        if width < 2 || height < 2 {
            return Ok(SurfaceBounds::default());
        }
        Ok(SurfaceBounds {
            x,
            y,
            width,
            height,
        })
    }

    fn register_surface_class() -> Result<(), SurfaceError> {
        // SAFETY: The class name is NUL-terminated and all handles are either
        // process-owned or valid stock handles.
        let instance = unsafe { GetModuleHandleW(ptr::null()) };
        if instance.is_null() {
            return Err(SurfaceError);
        }
        let window_class = WNDCLASSW {
            style: CS_HREDRAW | CS_VREDRAW,
            lpfnWndProc: Some(DefWindowProcW),
            cbClsExtra: 0,
            cbWndExtra: 0,
            hInstance: instance,
            hIcon: ptr::null_mut(),
            // SAFETY: IDC_ARROW is an integer resource identifier.
            hCursor: unsafe { LoadCursorW(ptr::null_mut(), IDC_ARROW) },
            // SAFETY: BLACK_BRUSH is a documented stock object identifier.
            hbrBackground: unsafe { GetStockObject(BLACK_BRUSH) },
            lpszMenuName: ptr::null(),
            lpszClassName: SURFACE_CLASS_NAME.as_ptr(),
        };
        // SAFETY: `window_class` remains valid for the duration of the call.
        let atom = unsafe { RegisterClassW(&window_class) };
        if atom == 0 {
            // A second registration in the same process is harmless; window
            // creation below is the authoritative availability check.
        }
        Ok(())
    }

    pub(super) fn create(parent_hwnd: isize) -> Result<VideoSurface, SurfaceError> {
        if parent_hwnd == 0 {
            return Err(SurfaceError);
        }
        register_surface_class()?;
        let parent = hwnd(parent_hwnd);
        // SAFETY: This is a side-effect-free validity check for a borrowed handle.
        if unsafe { IsWindow(parent) } == 0 {
            return Err(SurfaceError);
        }
        let instance = unsafe { GetModuleHandleW(ptr::null()) };
        if instance.is_null() {
            return Err(SurfaceError);
        }
        // SAFETY: The parent HWND belongs to Tauri's main window, the class is
        // registered above, and the empty title is a static NUL-terminated
        // string. The child starts hidden until the WebView reports bounds.
        let surface = unsafe {
            CreateWindowExW(
                0,
                SURFACE_CLASS_NAME.as_ptr(),
                SURFACE_CLASS_NAME.as_ptr(),
                WS_CHILD | WS_CLIPSIBLINGS | WS_CLIPCHILDREN,
                0,
                0,
                0,
                0,
                parent,
                ptr::null_mut(),
                instance,
                ptr::null(),
            )
        };
        if surface.is_null() {
            return Err(SurfaceError);
        }
        Ok(VideoSurface {
            parent_hwnd,
            hwnd: surface as isize,
            bounds: SurfaceBounds::default(),
            visible: false,
        })
    }

    pub(super) fn set_bounds(
        surface: &VideoSurface,
        bounds: SurfaceBounds,
        visible: bool,
    ) -> Result<(SurfaceBounds, bool), SurfaceError> {
        let parent = hwnd(surface.parent_hwnd);
        let child = hwnd(surface.hwnd);
        // SAFETY: Both calls only validate borrowed handles.
        if unsafe { IsWindow(parent) } == 0 || unsafe { IsWindow(child) } == 0 {
            return Err(SurfaceError);
        }
        let bounds = clamp_bounds(parent, bounds)?;
        let visible = visible && bounds.width > 0 && bounds.height > 0;
        if surface.bounds == bounds && surface.visible == visible {
            return Ok((bounds, false));
        }
        if !visible {
            let bounds = if bounds.width == 0 || bounds.height == 0 {
                surface.bounds
            } else {
                bounds
            };
            // SAFETY: Both handles are owned by the Tauri process.
            unsafe {
                ShowWindow(child, SW_HIDE);
            }
            return Ok((bounds, true));
        }
        // SAFETY: The handles are live; HWND_TOP affects sibling z-order only.
        let updated = unsafe {
            SetWindowPos(
                child,
                HWND_TOP,
                bounds.x,
                bounds.y,
                bounds.width,
                bounds.height,
                SWP_NOACTIVATE | SWP_SHOWWINDOW,
            )
        };
        if updated == 0 {
            return Err(SurfaceError);
        }
        // SAFETY: The handles are live and the call does not move or resize.
        unsafe {
            ShowWindow(child, SW_SHOWNOACTIVATE);
        }
        Ok((bounds, true))
    }

    pub(super) fn is_valid(surface: VideoSurface) -> bool {
        let parent = hwnd(surface.parent_hwnd);
        let child = hwnd(surface.hwnd);
        // SAFETY: All calls validate or inspect borrowed handles without
        // changing their state.
        if unsafe { IsWindow(parent) } == 0
            || unsafe { IsWindow(child) } == 0
            || unsafe { GetParent(child) } != parent
        {
            return false;
        }
        let mut parent_owner = 0;
        let mut child_owner = 0;
        unsafe {
            GetWindowThreadProcessId(parent, &mut parent_owner);
            GetWindowThreadProcessId(child, &mut child_owner);
        }
        let current_process = unsafe { GetCurrentProcessId() };
        parent_owner == current_process && child_owner == current_process
    }

    fn rect_values(rect: RECT) -> (i32, i32, i32, i32) {
        (rect.left, rect.top, rect.right, rect.bottom)
    }

    fn child_window_metrics(surface: HWND) -> Option<ChildWindowMetrics> {
        // SAFETY: `surface` is checked by the caller and the returned child is
        // a borrowed HWND valid for this inspection.
        let child = unsafe { FindWindowExW(surface, ptr::null_mut(), ptr::null(), ptr::null()) };
        if child.is_null() {
            return None;
        }
        let mut class_name = [0u16; 128];
        // SAFETY: The buffer is writable and sized to the supplied length.
        let class_len =
            unsafe { GetClassNameW(child, class_name.as_mut_ptr(), class_name.len() as i32) };
        let class_name = if class_len > 0 {
            String::from_utf16_lossy(&class_name[..class_len as usize])
        } else {
            String::new()
        };
        let mut rect = RECT::default();
        // SAFETY: `child` is a live child window and `rect` is writable.
        if unsafe { GetWindowRect(child, &mut rect) } == 0 {
            return None;
        }
        let mut client = RECT::default();
        // SAFETY: `child` is live and `client` is writable.
        if unsafe { GetClientRect(child, &mut client) } == 0 {
            return None;
        }
        // SAFETY: The APIs below are read-only for live HWNDs.
        let parent_matches = unsafe { GetParent(child) == surface };
        let style = unsafe { GetWindowLongPtrW(child, GWL_STYLE) as u32 };
        let visible = unsafe { IsWindowVisible(child) } != 0;
        let mut owner_pid = 0;
        unsafe {
            GetWindowThreadProcessId(child, &mut owner_pid);
        }
        Some(ChildWindowMetrics {
            class_name,
            owner_pid,
            parent_matches,
            style,
            visible,
            width: rect.right - rect.left,
            height: rect.bottom - rect.top,
            client_width: client.right - client.left,
            client_height: client.bottom - client.top,
        })
    }

    pub(super) fn diagnostics(
        parent_hwnd: isize,
        surface: Option<VideoSurface>,
    ) -> Result<NativeWindowMetrics, SurfaceError> {
        if parent_hwnd == 0 {
            return Err(SurfaceError);
        }
        let parent = hwnd(parent_hwnd);
        // SAFETY: This is a side-effect-free validity check for a borrowed handle.
        if unsafe { IsWindow(parent) } == 0 {
            return Err(SurfaceError);
        }
        let (client_width, client_height) = client_size(parent)?;
        let mut window_rect = RECT::default();
        // SAFETY: `parent` is live and `window_rect` is writable.
        if unsafe { GetWindowRect(parent, &mut window_rect) } == 0 {
            return Err(SurfaceError);
        }
        // SAFETY: The DPI API is side-effect free for a live window.
        let dpi = unsafe { GetDpiForWindow(parent) };
        // SAFETY: The window handle is live.
        let monitor: HMONITOR = unsafe { MonitorFromWindow(parent, MONITOR_DEFAULTTONEAREST) };
        let mut monitor_info = MONITORINFO {
            cbSize: std::mem::size_of::<MONITORINFO>() as u32,
            ..MONITORINFO::default()
        };
        // SAFETY: `monitor_info` has the required size and is writable.
        if monitor.is_null() || unsafe { GetMonitorInfoW(monitor, &mut monitor_info) } == 0 {
            return Err(SurfaceError);
        }
        let mpv_child = if let Some(surface) = surface {
            if surface.parent_hwnd != parent_hwnd {
                return Err(SurfaceError);
            }
            // SAFETY: This is a side-effect-free validity check for a borrowed handle.
            if unsafe { IsWindow(hwnd(surface.hwnd)) } == 0 {
                return Err(SurfaceError);
            }
            child_window_metrics(hwnd(surface.hwnd))
        } else {
            None
        };
        let (window_left, window_top, window_right, window_bottom) = rect_values(window_rect);
        let (monitor_left, monitor_top, monitor_right, monitor_bottom) =
            rect_values(monitor_info.rcMonitor);
        let (work_left, work_top, work_right, work_bottom) = rect_values(monitor_info.rcWork);
        Ok(NativeWindowMetrics {
            client_width,
            client_height,
            window_left,
            window_top,
            window_right,
            window_bottom,
            dpi,
            dpi_scale: f64::from(dpi) / 96.0,
            monitor_left,
            monitor_top,
            monitor_right,
            monitor_bottom,
            work_left,
            work_top,
            work_right,
            work_bottom,
            mpv_child,
        })
    }
}

#[cfg(not(windows))]
mod platform {
    use super::{NativeWindowMetrics, SurfaceBounds, SurfaceError, VideoSurface};

    pub(super) fn create(_parent_hwnd: isize) -> Result<VideoSurface, SurfaceError> {
        Err(SurfaceError)
    }

    pub(super) fn set_bounds(
        _surface: &VideoSurface,
        _bounds: SurfaceBounds,
        _visible: bool,
    ) -> Result<(SurfaceBounds, bool), SurfaceError> {
        Err(SurfaceError)
    }

    pub(super) fn is_valid(_surface: VideoSurface) -> bool {
        false
    }

    pub(super) fn diagnostics(
        _parent_hwnd: isize,
        _surface: Option<VideoSurface>,
    ) -> Result<NativeWindowMetrics, SurfaceError> {
        Err(SurfaceError)
    }
}
