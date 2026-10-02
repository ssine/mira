#!/usr/bin/env python3
"""Exercise real on-demand capture ownership/rate-limit/timeout behavior with API doubles.

Requires javac/java. This is not a substitute for acceptance on an Android device.
"""
import pathlib
import subprocess
import tempfile

ROOT = pathlib.Path(__file__).resolve().parents[1]
STUBS = {
    "android/os/Build.java": "package android.os; public class Build { public static class VERSION { public static int SDK_INT=35; } }",
    "android/os/Looper.java": "package android.os; public class Looper { private static final Looper MAIN=new Looper(); public static Looper getMainLooper(){return MAIN;} public static Looper myLooper(){return null;} }",
    "android/os/SystemClock.java": "package android.os; public class SystemClock { public static long elapsedRealtime(){return System.nanoTime()/1000000;} }",
    "android/view/Display.java": "package android.view; public class Display { public static final int DEFAULT_DISPLAY=0; }",
    "android/graphics/ColorSpace.java": "package android.graphics; public class ColorSpace {}",
    "android/hardware/HardwareBuffer.java": """
package android.hardware;
public class HardwareBuffer implements AutoCloseable {
    public static int live; private boolean closed;
    public HardwareBuffer(){live++;}
    public void close(){if(closed)throw new AssertionError("buffer double-close");closed=true;live--;}
}
""",
    "android/graphics/Bitmap.java": """
package android.graphics;
public class Bitmap {
    public static int live; public static boolean encode=true, failCopy=false;
    private boolean recycled;
    public enum Config { ARGB_8888 } public enum CompressFormat { PNG }
    public Bitmap(){live++;}
    public static Bitmap wrapHardwareBuffer(android.hardware.HardwareBuffer b, ColorSpace c){return new Bitmap();}
    public Bitmap copy(Config config, boolean mutable){if(failCopy)throw new IllegalStateException("copy failed");return new Bitmap();}
    public void recycle(){if(recycled)throw new AssertionError("bitmap double-free");recycled=true;live--;}
    public boolean compress(CompressFormat f,int q,java.io.OutputStream out){
        if(android.hardware.HardwareBuffer.live!=0)throw new AssertionError("hardware retained during PNG");
        return encode;
    }
    public int getWidth(){return 1172;} public int getHeight(){return 2748;}
}
""",
    "android/accessibilityservice/AccessibilityServiceInfo.java": """
package android.accessibilityservice;
public class AccessibilityServiceInfo {
    public static final int CAPABILITY_CAN_TAKE_SCREENSHOT=128;
    public int capabilities=128; public int getCapabilities(){return capabilities;}
}
""",
    "android/accessibilityservice/AccessibilityService.java": """
package android.accessibilityservice;
import java.util.concurrent.*;
public class AccessibilityService {
    public static final int ERROR_TAKE_SCREENSHOT_INTERVAL_TIME_SHORT=3;
    public final AccessibilityServiceInfo info=new AccessibilityServiceInfo();
    public int failures, failureCode=3; public boolean hold;
    public final java.util.List<Long> times=new java.util.ArrayList<>();
    public volatile Runnable pending;
    public CompletableFuture<Void> delivered=new CompletableFuture<>();
    public AccessibilityServiceInfo getServiceInfo(){return info;}
    public interface TakeScreenshotCallback {void onSuccess(ScreenshotResult result);void onFailure(int code);}
    public static class ScreenshotResult {
        final android.hardware.HardwareBuffer buffer=new android.hardware.HardwareBuffer();
        public android.hardware.HardwareBuffer getHardwareBuffer(){return buffer;}
        public android.graphics.ColorSpace getColorSpace(){return new android.graphics.ColorSpace();}
    }
    public void takeScreenshot(int display, Executor executor, TakeScreenshotCallback callback){
        // Model cold-start dispatch overhead before the platform sees the first request.
        if(times.isEmpty())try{Thread.sleep(90);}catch(InterruptedException e){throw new RuntimeException(e);}
        times.add(android.os.SystemClock.elapsedRealtime());
        if(failures-->0){executor.execute(()->callback.onFailure(failureCode));return;}
        Runnable deliver=()->executor.execute(()->{
            try {callback.onSuccess(new ScreenshotResult());delivered.complete(null);}
            catch(Throwable error){delivered.completeExceptionally(error);}
        });
        if(hold)pending=deliver;else deliver.run();
    }
}
""",
    "com/ssine/codexnode/MiraAccessibilityService.java": """
package com.ssine.codexnode;
public class MiraAccessibilityService {
    static android.accessibilityservice.AccessibilityService connected=new android.accessibilityservice.AccessibilityService();
    static android.accessibilityservice.AccessibilityService connectedService(){return connected;}
}
""",
    "com/ssine/codexnode/CaptureHarness.java": """
package com.ssine.codexnode;
import android.accessibilityservice.*;
import android.graphics.Bitmap;
import android.hardware.HardwareBuffer;
import java.util.concurrent.*;
public class CaptureHarness {
    static void check(boolean good,String message){if(!good)throw new AssertionError(message);}
    static void clean(){check(Bitmap.live==0 && HardwareBuffer.live==0,"resource leak: "+Bitmap.live+"/"+HardwareBuffer.live);}
    static void failure(String message) throws Exception {
        try {AccessibilityScreenCapture.screenshot();throw new AssertionError("failure expected");}
        catch(Exception expected){check(expected.toString().contains(message),expected.toString());}
        clean();
    }
    public static void main(String[] args) throws Exception {
        AccessibilityService service=MiraAccessibilityService.connected;
        check(AccessibilityScreenCapture.isReady(),"enabled capability");
        android.os.Build.VERSION.SDK_INT=29;check(!AccessibilityScreenCapture.isReady(),"old Android");
        android.os.Build.VERSION.SDK_INT=35;service.info.capabilities=0;
        check(!AccessibilityScreenCapture.isReady(),"missing capability");service.info.capabilities=128;
        MiraAccessibilityService.connected=null;check(!AccessibilityScreenCapture.isReady(),"disconnected");
        MiraAccessibilityService.connected=service;
        System.out.println("PASS version, connection and capability gates");
        AccessibilityScreenCapture.Screenshot shot=AccessibilityScreenCapture.screenshot();
        check(shot.width==1172 && shot.height==2748,"actual bitmap dimensions");clean();
        System.out.println("PASS capture and hardware release before encoding");
        service.failures=1;int before=service.times.size();AccessibilityScreenCapture.screenshot();clean();
        check(service.times.size()==before+2,"interval failure retried exactly once");
        for(int i=1;i<service.times.size();i++)check(service.times.get(i)-service.times.get(i-1)>=340,"rate spacing");
        service.failures=2;failure("Android code 3");
        service.failures=1;service.failureCode=6;failure("Android code 6");
        System.out.println("PASS rate spacing, bounded retry and platform failures");
        Bitmap.encode=false;failure("encode");Bitmap.encode=true;
        Bitmap.failCopy=true;failure("copy failed");Bitmap.failCopy=false;
        System.out.println("PASS copy and encoding failure cleanup");
        service.hold=true;service.delivered=new CompletableFuture<>();failure("TimeoutException");
        service.pending.run();service.delivered.get(2,TimeUnit.SECONDS);clean();service.hold=false;
        System.out.println("PASS timeout and late callback hardware cleanup");
        ExecutorService clients=Executors.newFixedThreadPool(2);
        try {
            Future<?> a=clients.submit(()->{try{AccessibilityScreenCapture.screenshot();}catch(Exception e){throw new RuntimeException(e);}});
            Future<?> b=clients.submit(()->{try{AccessibilityScreenCapture.screenshot();}catch(Exception e){throw new RuntimeException(e);}});
            a.get(10,TimeUnit.SECONDS);b.get(10,TimeUnit.SECONDS);clean();
        } finally {clients.shutdownNow();}
        System.out.println("PASS concurrent request serialization and recovery after timeout");
    }
}
""",
}

with tempfile.TemporaryDirectory(prefix="mira-accessibility-capture-") as temp:
    root = pathlib.Path(temp)
    for name, source in STUBS.items():
        target = root / name
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(source)
    sources = [str(p) for p in root.rglob("*.java")]
    sources.append(str(ROOT / "node/android/src/main/java/com/ssine/codexnode/AccessibilityScreenCapture.java"))
    subprocess.run(["javac", "-d", str(root), *sources], check=True)
    subprocess.run(["java", "-cp", str(root), "com.ssine.codexnode.CaptureHarness"], check=True, timeout=30)
