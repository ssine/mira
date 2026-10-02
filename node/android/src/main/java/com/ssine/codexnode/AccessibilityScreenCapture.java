package com.ssine.codexnode;

import android.accessibilityservice.AccessibilityService;
import android.accessibilityservice.AccessibilityServiceInfo;
import android.graphics.Bitmap;
import android.hardware.HardwareBuffer;
import android.os.Build;
import android.os.Looper;
import android.os.SystemClock;
import android.view.Display;

import java.io.ByteArrayOutputStream;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.ExecutionException;
import java.util.concurrent.Executor;
import java.util.concurrent.Executors;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.locks.ReentrantLock;

/** One-shot capture; never owns a MediaProjection session or a stream of frames. */
final class AccessibilityScreenCapture {
    private static final ReentrantLock captureLock = new ReentrantLock();
    private static final Executor callbacks = Executors.newSingleThreadExecutor(runnable -> {
        Thread thread = new Thread(runnable, "mira-accessibility-capture");
        thread.setDaemon(true);
        return thread;
    });
    private static long lastRequestAt = -1;

    static boolean isReady() {
        if (Build.VERSION.SDK_INT < 30) return false;
        AccessibilityService service = MiraAccessibilityService.connectedService();
        AccessibilityServiceInfo info = service == null ? null : service.getServiceInfo();
        return info != null && (info.getCapabilities()
                & AccessibilityServiceInfo.CAPABILITY_CAN_TAKE_SCREENSHOT) != 0;
    }

    static final class Screenshot {
        final byte[] png;
        final int width;
        final int height;

        Screenshot(byte[] png, int width, int height) {
            this.png = png;
            this.width = width;
            this.height = height;
        }
    }

    static Screenshot screenshot() throws Exception {
        if (Looper.myLooper() == Looper.getMainLooper()) {
            throw new IllegalStateException("screenshot must run off the main thread");
        }
        if (!captureLock.tryLock(5, TimeUnit.SECONDS)) {
            throw new Exception("accessibility screenshot busy; retry shortly");
        }
        try {
            // Android rate-limits this API. Space calls and retry an interval error once.
            for (int attempt = 0; attempt < 2; attempt++) {
                AccessibilityService service = MiraAccessibilityService.connectedService();
                if (!isReady() || service == null) {
                    throw new Exception("accessibility screenshot unavailable; enable Mira UI control");
                }
                long wait = 350 - (SystemClock.elapsedRealtime() - lastRequestAt);
                if (lastRequestAt >= 0 && wait > 0) Thread.sleep(wait);
                lastRequestAt = SystemClock.elapsedRealtime();
                try {
                    return request(service);
                } catch (CaptureFailure failure) {
                    if (failure.code != AccessibilityService.ERROR_TAKE_SCREENSHOT_INTERVAL_TIME_SHORT
                            || attempt != 0) throw failure;
                }
            }
            throw new AssertionError("unreachable");
        } finally {
            captureLock.unlock();
        }
    }

    private static Screenshot request(AccessibilityService service) throws Exception {
        CompletableFuture<Bitmap> result = new CompletableFuture<>();
        Bitmap bitmap = null;
        try {
            service.takeScreenshot(Display.DEFAULT_DISPLAY, callbacks,
                    new AccessibilityService.TakeScreenshotCallback() {
                @Override public void onSuccess(AccessibilityService.ScreenshotResult screenshot) {
                    Bitmap wrapped = null;
                    Bitmap copy = null;
                    try (HardwareBuffer buffer = screenshot.getHardwareBuffer()) {
                        if (result.isDone()) return;
                        wrapped = Bitmap.wrapHardwareBuffer(buffer, screenshot.getColorSpace());
                        if (wrapped == null) throw new IllegalStateException("cannot wrap screenshot buffer");
                        copy = wrapped.copy(Bitmap.Config.ARGB_8888, false);
                        if (copy == null) throw new IllegalStateException("cannot copy screenshot buffer");
                    } catch (RuntimeException error) {
                        if (copy != null) copy.recycle();
                        result.completeExceptionally(error);
                        return;
                    } finally {
                        if (wrapped != null) wrapped.recycle();
                    }
                    // A timeout can race this callback. Never leak a late bitmap.
                    if (!result.complete(copy)) copy.recycle();
                }

                @Override public void onFailure(int code) {
                    result.completeExceptionally(new CaptureFailure(code));
                }
            });
            try {
                bitmap = result.get(5, TimeUnit.SECONDS);
            } catch (ExecutionException error) {
                Throwable cause = error.getCause();
                if (cause instanceof Exception) throw (Exception) cause;
                throw new Exception("accessibility screenshot failed", cause);
            }
            ByteArrayOutputStream bytes = new ByteArrayOutputStream();
            if (!bitmap.compress(Bitmap.CompressFormat.PNG, 100, bytes)) {
                throw new Exception("could not encode accessibility screenshot");
            }
            return new Screenshot(bytes.toByteArray(), bitmap.getWidth(), bitmap.getHeight());
        } finally {
            if (bitmap != null) {
                bitmap.recycle();
            } else {
                result.cancel(false);
                // Also handles completion immediately before a timeout/interruption.
                result.whenComplete((late, error) -> { if (late != null) late.recycle(); });
            }
        }
    }

    private static final class CaptureFailure extends Exception {
        final int code;

        CaptureFailure(int code) {
            super("accessibility screenshot failed (Android code " + code + ")");
            this.code = code;
        }
    }
}
