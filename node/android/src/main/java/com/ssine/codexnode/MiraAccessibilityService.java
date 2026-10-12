package com.ssine.codexnode;

import android.accessibilityservice.AccessibilityService;
import android.accessibilityservice.GestureDescription;
import android.graphics.Path;
import android.graphics.Rect;
import android.os.Bundle;
import android.os.Handler;
import android.os.Looper;
import android.view.accessibility.AccessibilityEvent;
import android.view.accessibility.AccessibilityNodeInfo;

import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.concurrent.atomic.AtomicReference;

public final class MiraAccessibilityService extends AccessibilityService {
    private static volatile MiraAccessibilityService connected;
    private final Handler mainHandler = new Handler(Looper.getMainLooper());

    static boolean isConnected() {
        return connected != null;
    }

    static MiraAccessibilityService connectedService() {
        return connected;
    }

    @Override protected void onServiceConnected() {
        super.onServiceConnected();
        connected = this;
        // Some OEM builds do not deliver MY_PACKAGE_REPLACED to third-party
        // manifest receivers. Android reconnects an enabled accessibility
        // service after an app upgrade, which gives auto-start users a safe,
        // package-scoped recovery path on those devices.
        if (NodeConfig.load(this).autoStart
                && !MiraNodeService.isRunning()
                && !NodeConfig.isUserStopped(this)) {
            MiraNodeService.start(this);
        }
    }

    @Override public void onAccessibilityEvent(AccessibilityEvent event) {
    }

    @Override public void onInterrupt() {
    }

    @Override public void onDestroy() {
        if (connected == this) {
            connected = null;
        }
        super.onDestroy();
    }

    boolean tap(int x, int y) throws Exception {
        Path path = new Path();
        path.moveTo(x, y);
        return gesture(new GestureDescription.StrokeDescription(path, 0, 80));
    }

    boolean swipe(int startX, int startY, int endX, int endY, int durationMs) throws Exception {
        Path path = new Path();
        path.moveTo(startX, startY);
        path.lineTo(endX, endY);
        return gesture(new GestureDescription.StrokeDescription(path, 0, durationMs));
    }

    private boolean gesture(GestureDescription.StrokeDescription stroke) throws Exception {
        GestureDescription gesture = new GestureDescription.Builder().addStroke(stroke).build();
        CountDownLatch completed = new CountDownLatch(1);
        AtomicBoolean accepted = new AtomicBoolean(false);
        mainHandler.post(() -> {
            boolean dispatched = dispatchGesture(gesture, new GestureResultCallback() {
                @Override public void onCompleted(GestureDescription description) {
                    accepted.set(true);
                    completed.countDown();
                }

                @Override public void onCancelled(GestureDescription description) {
                    completed.countDown();
                }
            }, null);
            if (!dispatched) {
                completed.countDown();
            }
        });
        if (!completed.await(15, TimeUnit.SECONDS)) {
            throw new Exception("gesture timed out");
        }
        return accepted.get();
    }

    boolean globalKey(String keyCode) throws Exception {
        final int action;
        switch (keyCode) {
            case "KEYCODE_BACK": action = GLOBAL_ACTION_BACK; break;
            case "KEYCODE_HOME": action = GLOBAL_ACTION_HOME; break;
            case "KEYCODE_APP_SWITCH": action = GLOBAL_ACTION_RECENTS; break;
            case "KEYCODE_NOTIFICATION": action = GLOBAL_ACTION_NOTIFICATIONS; break;
            case "KEYCODE_SETTINGS": action = GLOBAL_ACTION_QUICK_SETTINGS; break;
            case "KEYCODE_POWER": action = GLOBAL_ACTION_POWER_DIALOG; break;
            default: throw new Exception("non-root mode only supports BACK, HOME, APP_SWITCH, NOTIFICATION, SETTINGS, and POWER keys");
        }
        CountDownLatch completed = new CountDownLatch(1);
        AtomicBoolean accepted = new AtomicBoolean(false);
        mainHandler.post(() -> {
            accepted.set(performGlobalAction(action));
            completed.countDown();
        });
        if (!completed.await(5, TimeUnit.SECONDS)) {
            throw new Exception("global action timed out");
        }
        return accepted.get();
    }

    boolean setText(String text) throws Exception {
        CountDownLatch completed = new CountDownLatch(1);
        AtomicBoolean accepted = new AtomicBoolean(false);
        AtomicReference<Exception> failure = new AtomicReference<>();
        mainHandler.post(() -> {
            AccessibilityNodeInfo root = null;
            AccessibilityNodeInfo focused = null;
            try {
                if (completed.getCount() == 0) return;
                root = getRootInActiveWindow();
                focused = root == null ? null : root.findFocus(AccessibilityNodeInfo.FOCUS_INPUT);
                if (focused == null || !focused.isEditable() || !focused.isEnabled()) {
                    throw new Exception("focused_editable_field_required");
                }
                Bundle arguments = new Bundle();
                arguments.putCharSequence(AccessibilityNodeInfo.ACTION_ARGUMENT_SET_TEXT_CHARSEQUENCE, text);
                accepted.set(focused.performAction(AccessibilityNodeInfo.ACTION_SET_TEXT, arguments));
                if (!accepted.get()) throw new Exception("focused_field_rejected_text");
            } catch (Exception error) {
                failure.set(error);
            } finally {
                if (focused != null && focused != root) focused.recycle();
                if (root != null) root.recycle();
                completed.countDown();
            }
        });
        if (!completed.await(5, TimeUnit.SECONDS)) {
            completed.countDown();
            throw new Exception("text action timed out");
        }
        if (failure.get() != null) throw failure.get();
        return accepted.get();
    }

    String focusedApp() throws Exception {
        CountDownLatch completed = new CountDownLatch(1);
        AtomicReference<String> result = new AtomicReference<>("");
        AtomicReference<Exception> failure = new AtomicReference<>();
        mainHandler.post(() -> {
            AccessibilityNodeInfo root = null;
            try {
                root = getRootInActiveWindow();
                if (root != null && root.getPackageName() != null) {
                    result.set(root.getPackageName().toString());
                } else throw new Exception("focused_app_unavailable");
            } catch (Exception error) {
                failure.set(error);
            } finally {
                if (root != null) root.recycle();
                completed.countDown();
            }
        });
        if (!completed.await(5, TimeUnit.SECONDS)) throw new Exception("focus status timed out");
        if (failure.get() != null) throw failure.get();
        return result.get();
    }

    String hierarchyXml() throws Exception {
        CountDownLatch completed = new CountDownLatch(1);
        StringBuilder result = new StringBuilder();
        mainHandler.post(() -> {
            AccessibilityNodeInfo root = getRootInActiveWindow();
            result.append("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<hierarchy>");
            if (root != null) {
                appendNode(result, root, 0, new int[] {0});
                root.recycle();
            }
            result.append("</hierarchy>");
            completed.countDown();
        });
        if (!completed.await(10, TimeUnit.SECONDS)) {
            throw new Exception("hierarchy capture timed out");
        }
        return result.toString();
    }

    private static void appendNode(StringBuilder output, AccessibilityNodeInfo node, int depth,
                                   int[] count) {
        if (depth > 80 || count[0]++ > 10000) {
            return;
        }
        Rect bounds = new Rect();
        node.getBoundsInScreen(bounds);
        output.append("<node class=\"").append(xml(node.getClassName()))
                .append("\" package=\"").append(xml(node.getPackageName()))
                .append("\" text=\"").append(xml(node.getText()))
                .append("\" content-desc=\"").append(xml(node.getContentDescription()))
                .append("\" view-id=\"").append(xml(node.getViewIdResourceName()))
                .append("\" bounds=\"").append(bounds.toShortString())
                .append("\" clickable=\"").append(node.isClickable())
                .append("\" editable=\"").append(node.isEditable())
                .append("\" enabled=\"").append(node.isEnabled()).append("\">");
        for (int index = 0; index < node.getChildCount(); index++) {
            AccessibilityNodeInfo child = node.getChild(index);
            if (child != null) {
                appendNode(output, child, depth + 1, count);
                child.recycle();
            }
        }
        output.append("</node>");
    }

    private static String xml(CharSequence value) {
        if (value == null) {
            return "";
        }
        StringBuilder output = new StringBuilder();
        for (int index = 0; index < value.length(); index++) {
            char character = value.charAt(index);
            switch (character) {
                case '&': output.append("&amp;"); break;
                case '<': output.append("&lt;"); break;
                case '>': output.append("&gt;"); break;
                case '"': output.append("&quot;"); break;
                case '\'': output.append("&apos;"); break;
                default:
                    if (character >= 0x20 || character == '\n' || character == '\t') {
                        output.append(character);
                    }
            }
        }
        return output.toString();
    }
}
