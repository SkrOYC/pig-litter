import ctypes
import plistlib


def native_windows():
    graphics = ctypes.CDLL("/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics")
    foundation = ctypes.CDLL("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation")
    graphics.CGWindowListCopyWindowInfo.argtypes = [ctypes.c_uint32, ctypes.c_uint32]
    graphics.CGWindowListCopyWindowInfo.restype = ctypes.c_void_p
    foundation.CFPropertyListCreateData.argtypes = [ctypes.c_void_p, ctypes.c_void_p, ctypes.c_long, ctypes.c_ulong, ctypes.POINTER(ctypes.c_void_p)]
    foundation.CFPropertyListCreateData.restype = ctypes.c_void_p
    foundation.CFDataGetLength.argtypes = [ctypes.c_void_p]
    foundation.CFDataGetLength.restype = ctypes.c_long
    foundation.CFDataGetBytePtr.argtypes = [ctypes.c_void_p]
    foundation.CFDataGetBytePtr.restype = ctypes.POINTER(ctypes.c_ubyte)
    foundation.CFRelease.argtypes = [ctypes.c_void_p]
    windows = graphics.CGWindowListCopyWindowInfo(0, 0)
    if not windows:
        raise RuntimeError("Core Graphics did not return a window census")
    data = None
    error = ctypes.c_void_p()
    try:
        data = foundation.CFPropertyListCreateData(None, windows, 200, 0, ctypes.byref(error))
        if not data:
            raise RuntimeError("Core Graphics window census could not be serialized")
        content = ctypes.string_at(foundation.CFDataGetBytePtr(data), foundation.CFDataGetLength(data))
        values = plistlib.loads(content)
        if not isinstance(values, list):
            raise RuntimeError("Core Graphics returned an unexpected window census")
        records = []
        for value in values:
            if not isinstance(value, dict) or not isinstance(value.get("kCGWindowNumber"), int) or not isinstance(value.get("kCGWindowOwnerPID"), int):
                raise RuntimeError("Core Graphics window identity is incomplete")
            records.append({"number": value["kCGWindowNumber"], "owner_pid": value["kCGWindowOwnerPID"], "onscreen": value.get("kCGWindowIsOnscreen")})
        return records
    finally:
        if error.value:
            foundation.CFRelease(error)
        if data:
            foundation.CFRelease(data)
        foundation.CFRelease(windows)
