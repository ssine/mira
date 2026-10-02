package com.ssine.codexnode;

import android.app.Service;
import android.content.Context;
import android.content.Intent;
import android.graphics.Bitmap;
import android.graphics.PixelFormat;
import android.hardware.display.DisplayManager;
import android.hardware.display.VirtualDisplay;
import android.media.Image;
import android.media.ImageReader;
import android.media.projection.MediaProjection;
import android.media.projection.MediaProjectionManager;
import android.os.Handler;
import android.os.HandlerThread;
import android.os.SystemClock;
import android.util.DisplayMetrics;
import android.util.Log;
import android.view.WindowManager;

import java.io.ByteArrayOutputStream;
import java.nio.ByteBuffer;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.locks.ReentrantLock;

final class ScreenCaptureController {
    private static volatile ScreenCaptureController active;

    private final Object imageLock = new Object();
    private final ReentrantLock screenshotLock = new ReentrantLock();
    private HandlerThread imageThread;
    private volatile MediaProjection projection;
    private VirtualDisplay virtualDisplay;
    private ImageReader imageReader;
    private Image latestImage;
    private int width;
    private int height;
    private int density;

    static boolean isReady() {
        ScreenCaptureController value = active;
        return value != null && value.projection != null;
    }

    synchronized void configure(Service service, int resultCode, Intent resultData) throws Exception {
        close();
        imageThread = new HandlerThread("codex-screen-capture");
        imageThread.start();
        Handler handler = new Handler(imageThread.getLooper());
        DisplayMetrics metrics = new DisplayMetrics();
        WindowManager windows = (WindowManager) service.getSystemService(Context.WINDOW_SERVICE);
        windows.getDefaultDisplay().getRealMetrics(metrics);
        width = metrics.widthPixels;
        height = metrics.heightPixels;
        density = metrics.densityDpi;
        // One retained frame plus the two temporary slots acquireLatestImage
        // needs to discard old frames. No Image may escape imageLock below.
        imageReader = ImageReader.newInstance(width, height, PixelFormat.RGBA_8888, 3);
        imageReader.setOnImageAvailableListener(reader -> {
            synchronized (imageLock) {
                if (reader != imageReader || projection == null) {
                    return;
                }
                Image image;
                try {
                    image = reader.acquireLatestImage();
                } catch (IllegalStateException error) {
                    // A late/invalid reader callback must not kill the whole
                    // Node and with it the user's MediaProjection grant.
                    Log.w("MiraScreenCapture", "Could not acquire a screen frame", error);
                    return;
                }
                if (image == null) {
                    return;
                }
                if (latestImage != null) {
                    latestImage.close();
                }
                latestImage = image;
                imageLock.notifyAll();
            }
        }, handler);

        MediaProjectionManager manager = (MediaProjectionManager)
                service.getSystemService(Context.MEDIA_PROJECTION_SERVICE);
        projection = manager.getMediaProjection(resultCode, resultData);
        if (projection == null) {
            throw new Exception("Android did not create a MediaProjection session");
        }
        final MediaProjection grantedProjection = projection;
        projection.registerCallback(new MediaProjection.Callback() {
            @Override public void onStop() {
                synchronized (ScreenCaptureController.this) {
                    // An old session can report onStop after a new grant.
                    if (projection == grantedProjection) {
                        close();
                    }
                }
            }
        }, handler);
        virtualDisplay = projection.createVirtualDisplay("Mira Node capture", width, height,
                density, DisplayManager.VIRTUAL_DISPLAY_FLAG_AUTO_MIRROR,
                imageReader.getSurface(), null, handler);
        active = this;
    }

    int width() {
        return width;
    }

    int height() {
        return height;
    }

    byte[] screenshot() throws Exception {
        if (!screenshotLock.tryLock(5, TimeUnit.SECONDS)) {
            throw new Exception("screen capture busy");
        }
        try {
            return captureScreenshot();
        } finally {
            screenshotLock.unlock();
        }
    }

    private byte[] captureScreenshot() throws Exception {
        Bitmap bitmap;
        synchronized (imageLock) {
            MediaProjection expectedProjection = projection;
            if (expectedProjection == null) {
                throw new Exception("screen_capture_permission_required");
            }
            long deadline = SystemClock.elapsedRealtime() + 5000;
            while (latestImage == null && projection == expectedProjection
                    && SystemClock.elapsedRealtime() < deadline) {
                imageLock.wait(Math.max(1, deadline - SystemClock.elapsedRealtime()));
            }
            if (projection != expectedProjection) {
                throw new Exception("screen_capture_permission_required");
            }
            Image image = latestImage;
            latestImage = null;
            if (image == null) {
                throw new Exception("screen capture timed out");
            }
            // Copy and release while acquisition/reader teardown are excluded.
            // PNG encoding can be slow; it must never retain an Image slot.
            try {
                bitmap = copyBitmap(image);
            } finally {
                image.close();
            }
        }
        try {
            ByteArrayOutputStream output = new ByteArrayOutputStream();
            if (!bitmap.compress(Bitmap.CompressFormat.PNG, 100, output)) {
                throw new Exception("could not encode screenshot");
            }
            return output.toByteArray();
        } finally {
            bitmap.recycle();
        }
    }

    private Bitmap copyBitmap(Image image) {
        Image.Plane plane = image.getPlanes()[0];
        ByteBuffer buffer = plane.getBuffer();
        int pixelStride = plane.getPixelStride();
        int rowStride = plane.getRowStride();
        int paddedWidth = width + (rowStride - pixelStride * width) / pixelStride;
        Bitmap padded = Bitmap.createBitmap(paddedWidth, height, Bitmap.Config.ARGB_8888);
        Bitmap cropped = null;
        try {
            padded.copyPixelsFromBuffer(buffer);
            cropped = Bitmap.createBitmap(padded, 0, 0, width, height);
            return cropped;
        } finally {
            // createBitmap may return its input when no cropping is needed.
            if (cropped != padded) {
                padded.recycle();
            }
        }
    }

    synchronized void close() {
        if (active == this) {
            active = null;
        }
        MediaProjection previousProjection;
        synchronized (imageLock) {
            previousProjection = projection;
            projection = null;
            if (latestImage != null) {
                latestImage.close();
                latestImage = null;
            }
            if (imageReader != null) {
                imageReader.setOnImageAvailableListener(null, null);
                imageReader.close();
                imageReader = null;
            }
            imageLock.notifyAll();
        }
        if (virtualDisplay != null) {
            virtualDisplay.release();
            virtualDisplay = null;
        }
        if (previousProjection != null) {
            previousProjection.stop();
        }
        if (imageThread != null && imageThread.isAlive()) {
            imageThread.quitSafely();
        }
        imageThread = null;
    }
}
