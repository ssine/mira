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
import android.util.DisplayMetrics;
import android.view.WindowManager;

import java.io.ByteArrayOutputStream;
import java.nio.ByteBuffer;

final class ScreenCaptureController {
    private static volatile ScreenCaptureController active;

    private final Object imageLock = new Object();
    private HandlerThread imageThread;
    private MediaProjection projection;
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
        imageReader = ImageReader.newInstance(width, height, PixelFormat.RGBA_8888, 2);
        imageReader.setOnImageAvailableListener(reader -> {
            Image image = reader.acquireLatestImage();
            if (image == null) {
                return;
            }
            synchronized (imageLock) {
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
        projection.registerCallback(new MediaProjection.Callback() {
            @Override public void onStop() {
                active = null;
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
        if (projection == null) {
            throw new Exception("screen_capture_permission_required");
        }
        Image image;
        synchronized (imageLock) {
            long deadline = System.currentTimeMillis() + 5000;
            while (latestImage == null && System.currentTimeMillis() < deadline) {
                imageLock.wait(Math.max(1, deadline - System.currentTimeMillis()));
            }
            image = latestImage;
            latestImage = null;
        }
        if (image == null) {
            throw new Exception("screen capture timed out");
        }
        try {
            Image.Plane plane = image.getPlanes()[0];
            ByteBuffer buffer = plane.getBuffer();
            int pixelStride = plane.getPixelStride();
            int rowStride = plane.getRowStride();
            int rowPadding = rowStride - pixelStride * width;
            int paddedWidth = width + rowPadding / pixelStride;
            Bitmap padded = Bitmap.createBitmap(paddedWidth, height, Bitmap.Config.ARGB_8888);
            padded.copyPixelsFromBuffer(buffer);
            Bitmap cropped = Bitmap.createBitmap(padded, 0, 0, width, height);
            ByteArrayOutputStream output = new ByteArrayOutputStream();
            if (!cropped.compress(Bitmap.CompressFormat.PNG, 100, output)) {
                throw new Exception("could not encode screenshot");
            }
            padded.recycle();
            cropped.recycle();
            return output.toByteArray();
        } finally {
            image.close();
        }
    }

    synchronized void close() {
        active = null;
        synchronized (imageLock) {
            if (latestImage != null) {
                latestImage.close();
                latestImage = null;
            }
        }
        if (virtualDisplay != null) {
            virtualDisplay.release();
            virtualDisplay = null;
        }
        if (imageReader != null) {
            imageReader.close();
            imageReader = null;
        }
        if (projection != null) {
            projection.stop();
            projection = null;
        }
        if (imageThread != null && imageThread.isAlive()) {
            imageThread.quitSafely();
        }
        imageThread = null;
    }
}
