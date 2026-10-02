#!/usr/bin/env python3
"""Run the real capture controller against a bounded ImageReader test double.

Requires a JDK (javac/java). This deterministically exercises frame ownership and
session races; it is not a substitute for Android MediaProjection device tests.
An optional source path lets the same regression reproduce the pre-fix crash.
"""

import pathlib
import subprocess
import sys
import tempfile


ROOT = pathlib.Path(__file__).resolve().parents[1]
SOURCE = pathlib.Path(sys.argv[1]) if len(sys.argv) > 1 else ROOT / (
    "node/android/src/main/java/com/ssine/codexnode/ScreenCaptureController.java"
)

STUBS = {
    "android/content/Intent.java": "package android.content; public class Intent {}",
    "android/content/Context.java": """
package android.content;
public class Context {
    public static final String WINDOW_SERVICE = "window", MEDIA_PROJECTION_SERVICE = "projection";
    public Object getSystemService(String name) {
        return name.equals(WINDOW_SERVICE) ? new android.view.WindowManager()
            : new android.media.projection.MediaProjectionManager();
    }
}
""",
    "android/app/Service.java": "package android.app; public class Service extends android.content.Context {}",
    "android/graphics/PixelFormat.java": "package android.graphics; public class PixelFormat { public static final int RGBA_8888 = 1; }",
    "android/util/DisplayMetrics.java": "package android.util; public class DisplayMetrics { public int widthPixels=2, heightPixels=2, densityDpi=160; }",
    "android/view/WindowManager.java": """
package android.view;
public class WindowManager {
    public WindowManager getDefaultDisplay() { return this; }
    public void getRealMetrics(android.util.DisplayMetrics metrics) {}
}
""",
    "android/os/Looper.java": "package android.os; public class Looper {}",
    "android/os/Handler.java": "package android.os; public class Handler { public Handler(Looper looper) {} }",
    "android/os/HandlerThread.java": """
package android.os;
public class HandlerThread extends Thread {
    public HandlerThread(String name) { super(name); }
    public Looper getLooper() { return new Looper(); }
    public boolean quitSafely() { return true; }
}
""",
    "android/os/SystemClock.java": "package android.os; public class SystemClock { public static long elapsedRealtime() { return System.nanoTime()/1000000; } }",
    "android/util/Log.java": "package android.util; public class Log { public static int w(String tag, String msg, Throwable error) { return 0; } }",
    "android/hardware/display/DisplayManager.java": "package android.hardware.display; public class DisplayManager { public static final int VIRTUAL_DISPLAY_FLAG_AUTO_MIRROR=1; }",
    "android/hardware/display/VirtualDisplay.java": "package android.hardware.display; public class VirtualDisplay { public void release() {} }",
    "android/media/projection/MediaProjectionManager.java": """
package android.media.projection;
public class MediaProjectionManager {
    public static MediaProjection last;
    public MediaProjection getMediaProjection(int code, android.content.Intent data) {
        return last = new MediaProjection();
    }
}
""",
    "android/media/projection/MediaProjection.java": """
package android.media.projection;
public class MediaProjection {
    public static abstract class Callback { public abstract void onStop(); }
    public Callback callback;
    public void registerCallback(Callback value, android.os.Handler handler) { callback=value; }
    public android.hardware.display.VirtualDisplay createVirtualDisplay(
            String name, int w, int h, int d, int flags, Object surface,
            Object callback, android.os.Handler handler) {
        return new android.hardware.display.VirtualDisplay();
    }
    // Model asynchronous onStop delivery: the harness invokes callback later.
    public void stop() {}
}
""",
    "android/media/ImageReader.java": """
package android.media;
public class ImageReader {
    public interface OnImageAvailableListener { void onImageAvailable(ImageReader reader); }
    public static ImageReader last;
    public OnImageAvailableListener listener;
    public final java.util.concurrent.atomic.AtomicInteger acquired = new java.util.concurrent.atomic.AtomicInteger();
    public int maxImages, queued;
    public boolean closed, failAcquire;
    public static ImageReader newInstance(int w, int h, int format, int maxImages) {
        last = new ImageReader(); last.maxImages=maxImages; return last;
    }
    public void setOnImageAvailableListener(OnImageAvailableListener value, android.os.Handler handler) { listener=value; }
    public Object getSurface() { return this; }
    public synchronized Image acquireLatestImage() {
        if (closed || failAcquire || acquired.get() >= maxImages)
            throw new IllegalStateException("maxImages ("+maxImages+") has already been acquired");
        if (queued == 0) return null;
        queued--; acquired.incrementAndGet(); return new Image(this);
    }
    public void frame() {
        synchronized (this) { queued++; }
        if (listener != null) listener.onImageAvailable(this);
    }
    public void close() { closed=true; }
}
""",
    "android/media/Image.java": """
package android.media;
public class Image {
    private final ImageReader owner;
    private boolean closed;
    Image(ImageReader owner) { this.owner=owner; }
    public Plane[] getPlanes() {
        if (closed || owner.closed) throw new IllegalStateException("image already closed");
        return new Plane[] {new Plane()};
    }
    public void close() { if (!closed) { closed=true; owner.acquired.decrementAndGet(); } }
    public static class Plane {
        public java.nio.ByteBuffer getBuffer() { return java.nio.ByteBuffer.allocate(16); }
        public int getPixelStride() { return 4; }
        public int getRowStride() { return 8; }
    }
}
""",
    "android/graphics/Bitmap.java": """
package android.graphics;
public class Bitmap {
    public enum Config { ARGB_8888 }
    public enum CompressFormat { PNG }
    public static volatile java.util.concurrent.CountDownLatch encoding, release;
    public static Bitmap createBitmap(int w, int h, Config config) { return new Bitmap(); }
    public static Bitmap createBitmap(Bitmap source, int x, int y, int w, int h) { return source; }
    public void copyPixelsFromBuffer(java.nio.Buffer buffer) {}
    public boolean compress(CompressFormat format, int quality, java.io.OutputStream output) {
        try {
            if (encoding != null) { encoding.countDown(); release.await(); }
            output.write(42); return true;
        } catch (Exception error) { throw new RuntimeException(error); }
    }
    public void recycle() {}
}
""",
    "com/ssine/codexnode/CaptureRegression.java": """
package com.ssine.codexnode;
import android.app.Service;
import android.content.Intent;
import android.graphics.Bitmap;
import android.media.ImageReader;
import android.media.projection.MediaProjection;
import android.media.projection.MediaProjectionManager;
import java.util.concurrent.*;

public class CaptureRegression {
    static void check(boolean value, String message) {
        if (!value) throw new AssertionError(message);
    }
    static ScreenCaptureController configured() throws Exception {
        ScreenCaptureController capture = new ScreenCaptureController();
        capture.configure(new Service(), -1, new Intent());
        return capture;
    }
    public static void main(String[] args) throws Exception {
        ExecutorService workers = Executors.newCachedThreadPool();
        try {
            ScreenCaptureController capture = configured();
            ImageReader reader = ImageReader.last;
            reader.frame();
            Bitmap.encoding = new CountDownLatch(1);
            Bitmap.release = new CountDownLatch(1);
            Future<byte[]> result = workers.submit(capture::screenshot);
            check(Bitmap.encoding.await(2, TimeUnit.SECONDS), "encoding did not start");
            try {
                // The old controller crashes on the second frame while PNG
                // compression owns one Image and latestImage owns another.
                for (int i=0; i<100; i++) reader.frame();
                check(reader.acquired.get()==1, "encoding retained an Image slot");
            } finally { Bitmap.release.countDown(); }
            check(result.get(2, TimeUnit.SECONDS)[0]==42, "screenshot failed");
            Bitmap.encoding=null;
            capture.close();
            check(reader.closed && reader.acquired.get()==0, "close leaked reader/images");
            System.out.println("PASS frames arriving during slow PNG encoding");

            capture = configured();
            ImageReader oldReader = ImageReader.last;
            ImageReader.OnImageAvailableListener oldListener = oldReader.listener;
            MediaProjection oldProjection = MediaProjectionManager.last;
            capture.configure(new Service(), -1, new Intent());
            oldListener.onImageAvailable(oldReader);
            oldProjection.callback.onStop();
            check(ScreenCaptureController.isReady(), "old callback revoked new grant");
            capture.close();
            System.out.println("PASS stale image and projection callbacks after regrant");

            capture = configured();
            reader = ImageReader.last;
            reader.failAcquire=true;
            reader.frame();
            reader.failAcquire=false;
            reader.frame();
            check(capture.screenshot()[0]==42, "capture failed after transient acquisition error");
            capture.close();
            System.out.println("PASS acquisition exception does not escape callback");

            capture = configured();
            ScreenCaptureController waitingCapture = capture;
            CountDownLatch started = new CountDownLatch(1);
            Future<String> waiting = workers.submit(() -> {
                started.countDown();
                try { waitingCapture.screenshot(); return "unexpected success"; }
                catch (Exception error) { return error.getMessage(); }
            });
            check(started.await(1, TimeUnit.SECONDS), "waiter did not start");
            Thread.sleep(50);
            reader=ImageReader.last;
            MediaProjectionManager.last.callback.onStop();
            check(waiting.get(1, TimeUnit.SECONDS).equals("screen_capture_permission_required"),
                "stop did not wake pending screenshot");
            check(!ScreenCaptureController.isReady() && reader.closed, "stop leaked session");
            System.out.println("PASS projection stop wakes waiter and releases resources");

            capture=configured();
            ScreenCaptureController concurrentCapture=capture;
            reader=ImageReader.last;
            java.util.List<Future<byte[]>> concurrent=new java.util.ArrayList<>();
            for(int i=0; i<8; i++) concurrent.add(workers.submit(concurrentCapture::screenshot));
            for(int i=0; i<80; i++) { reader.frame(); Thread.sleep(2); }
            for(Future<byte[]> value:concurrent) check(value.get(2, TimeUnit.SECONDS)[0]==42, "concurrent screenshot failed");
            capture.close();
            check(reader.acquired.get()==0, "concurrent screenshots leaked images");
            System.out.println("PASS concurrent screenshots share bounded Image slots");
        } finally {
            if(Bitmap.release!=null) Bitmap.release.countDown();
            workers.shutdownNow();
        }
    }
}
""",
}


def main():
    with tempfile.TemporaryDirectory(prefix="mira-capture-regression-") as temporary:
        directory = pathlib.Path(temporary)
        for name, text in STUBS.items():
            target = directory / name
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text(text, encoding="utf-8")
        target = directory / "com/ssine/codexnode/ScreenCaptureController.java"
        target.write_text(SOURCE.read_text(encoding="utf-8"), encoding="utf-8")
        subprocess.run(["javac", "-d", str(directory / "classes"),
                        *map(str, directory.rglob("*.java"))], check=True)
        subprocess.run(["java", "-cp", str(directory / "classes"),
                        "com.ssine.codexnode.CaptureRegression"], check=True, timeout=30)


if __name__ == "__main__":
    main()
