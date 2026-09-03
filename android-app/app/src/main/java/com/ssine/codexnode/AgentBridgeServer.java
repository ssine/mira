package com.ssine.codexnode;

import android.content.Context;
import android.os.Build;
import android.os.Environment;
import android.util.Base64;
import android.util.DisplayMetrics;
import android.view.WindowManager;

import org.json.JSONObject;

import java.io.BufferedInputStream;
import java.io.BufferedOutputStream;
import java.io.ByteArrayOutputStream;
import java.io.EOFException;
import java.net.InetAddress;
import java.net.ServerSocket;
import java.net.Socket;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.util.Locale;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;

final class AgentBridgeServer {
    private static final int MAX_REQUEST_BYTES = 1024 * 1024;
    private final Context context;
    private final ScreenCaptureController capture;
    private final String token;
    private final ExecutorService clients = Executors.newCachedThreadPool();
    private ServerSocket server;
    private Thread acceptThread;

    AgentBridgeServer(Context context, ScreenCaptureController capture, String token) {
        this.context = context;
        this.capture = capture;
        this.token = token;
    }

    int start() throws Exception {
        server = new ServerSocket(0, 16, InetAddress.getByName("127.0.0.1"));
        acceptThread = new Thread(this::acceptLoop, "codex-android-bridge");
        acceptThread.start();
        return server.getLocalPort();
    }

    void close() {
        try {
            if (server != null) {
                server.close();
            }
        } catch (Exception ignored) {
        }
        clients.shutdownNow();
    }

    private void acceptLoop() {
        while (server != null && !server.isClosed()) {
            try {
                Socket socket = server.accept();
                clients.execute(() -> handle(socket));
            } catch (Exception error) {
                if (server != null && !server.isClosed()) {
                    AgentService.publishStatus(context, "Android bridge failed: " + error.getMessage(), null);
                }
            }
        }
    }

    private void handle(Socket socket) {
        try (Socket ignored = socket;
             BufferedInputStream input = new BufferedInputStream(socket.getInputStream());
             BufferedOutputStream output = new BufferedOutputStream(socket.getOutputStream())) {
            socket.setSoTimeout(35000);
            String requestLine = readLine(input);
            if (requestLine == null) {
                return;
            }
            String[] requestParts = requestLine.split(" ");
            int contentLength = 0;
            String authorization = "";
            while (true) {
                String line = readLine(input);
                if (line == null || line.isEmpty()) {
                    break;
                }
                int separator = line.indexOf(':');
                if (separator < 0) {
                    continue;
                }
                String name = line.substring(0, separator).trim().toLowerCase(Locale.ROOT);
                String value = line.substring(separator + 1).trim();
                if (name.equals("content-length")) {
                    contentLength = Integer.parseInt(value);
                } else if (name.equals("authorization")) {
                    authorization = value;
                }
            }
            if (requestParts.length < 2 || !requestParts[0].equals("POST")
                    || !requestParts[1].equals("/v1/screen")) {
                respond(output, 404, error("not_found"));
                return;
            }
            byte[] expected = ("Bearer " + token).getBytes(StandardCharsets.UTF_8);
            if (!MessageDigest.isEqual(expected, authorization.getBytes(StandardCharsets.UTF_8))) {
                respond(output, 401, error("unauthorized"));
                return;
            }
            if (contentLength < 0 || contentLength > MAX_REQUEST_BYTES) {
                respond(output, 413, error("request_too_large"));
                return;
            }
            byte[] body = readBytes(input, contentLength);
            try {
                JSONObject result = screen(new JSONObject(new String(body, StandardCharsets.UTF_8)));
                respond(output, 200, result);
            } catch (PermissionException error) {
                respond(output, 409, error(error.getMessage()));
            } catch (Exception error) {
                respond(output, 400, error(error.getMessage()));
            }
        } catch (Exception ignored) {
        }
    }

    private JSONObject screen(JSONObject params) throws Exception {
        String action = params.optString("action", "");
        DisplayMetrics display = displayMetrics();
        switch (action) {
            case "permissions":
                return new JSONObject().put("accessibility",
                                CodexAccessibilityService.isConnected() ? "ready" : "user_grant_required")
                        .put("screenCapture", ScreenCaptureController.isReady()
                                ? "ready" : "user_grant_required")
                        .put("sharedFiles", Build.VERSION.SDK_INT < 30 || Environment.isExternalStorageManager()
                                ? "all_files" : "app_sandbox")
                        .put("root", false);
            case "display":
                return new JSONObject().put("action", "display")
                        .put("width", display.widthPixels).put("height", display.heightPixels)
                        .put("sizeOutput", "Android API: " + display.widthPixels + "x" + display.heightPixels)
                        .put("densityOutput", "Android API: " + display.densityDpi);
            case "screenshot": {
                if (!ScreenCaptureController.isReady()) {
                    throw new PermissionException("screen_capture_permission_required");
                }
                byte[] png = capture.screenshot();
                if (png.length > 10 * 1024 * 1024) {
                    throw new Exception("screenshot exceeds 10 MiB");
                }
                return new JSONObject().put("action", "screenshot")
                        .put("mimeType", "image/png").put("encoding", "base64")
                        .put("content", Base64.encodeToString(png, Base64.NO_WRAP))
                        .put("bytes", png.length).put("width", capture.width())
                        .put("height", capture.height()).put("capturedAt", System.currentTimeMillis());
            }
            case "hierarchy": {
                CodexAccessibilityService service = requireAccessibility();
                return new JSONObject().put("action", "hierarchy")
                        .put("format", "accessibility-xml").put("content", service.hierarchyXml());
            }
            case "tap": {
                int x = coordinate(params, "x", display.widthPixels);
                int y = coordinate(params, "y", display.heightPixels);
                boolean accepted = requireAccessibility().tap(x, y);
                return new JSONObject().put("action", "tap").put("x", x).put("y", y)
                        .put("accepted", accepted);
            }
            case "swipe": {
                int startX = coordinate(params, "startX", display.widthPixels);
                int startY = coordinate(params, "startY", display.heightPixels);
                int endX = coordinate(params, "endX", display.widthPixels);
                int endY = coordinate(params, "endY", display.heightPixels);
                int duration = params.optInt("durationMs", 300);
                if (duration < 1 || duration > 60000) {
                    throw new Exception("durationMs must be between 1 and 60000");
                }
                boolean accepted = requireAccessibility().swipe(startX, startY, endX, endY, duration);
                return new JSONObject().put("action", "swipe").put("startX", startX)
                        .put("startY", startY).put("endX", endX).put("endY", endY)
                        .put("durationMs", duration).put("accepted", accepted);
            }
            case "key": {
                Object raw = params.opt("keyCode");
                if (!(raw instanceof String)) {
                    throw new Exception("non-root keyCode must be a supported KEYCODE_* name");
                }
                String keyCode = (String) raw;
                boolean accepted = requireAccessibility().globalKey(keyCode);
                return new JSONObject().put("action", "key").put("keyCode", keyCode)
                        .put("accepted", accepted);
            }
            case "text": {
                String text = params.optString("text", "");
                if (text.isEmpty() || text.length() > 4096) {
                    throw new Exception("text must contain between 1 and 4096 characters");
                }
                boolean accepted = requireAccessibility().setText(text);
                return new JSONObject().put("action", "text").put("characters", text.length())
                        .put("accepted", accepted);
            }
            default:
                throw new Exception("unsupported screen action: " + action);
        }
    }

    private CodexAccessibilityService requireAccessibility() throws PermissionException {
        CodexAccessibilityService service = CodexAccessibilityService.connectedService();
        if (service == null) {
            throw new PermissionException("accessibility_permission_required");
        }
        return service;
    }

    private DisplayMetrics displayMetrics() {
        DisplayMetrics metrics = new DisplayMetrics();
        WindowManager windows = (WindowManager) context.getSystemService(Context.WINDOW_SERVICE);
        windows.getDefaultDisplay().getRealMetrics(metrics);
        return metrics;
    }

    private static int coordinate(JSONObject params, String name, int maximum) throws Exception {
        if (!params.has(name)) {
            throw new Exception(name + " is required");
        }
        int value = params.getInt(name);
        if (value < 0 || value >= maximum) {
            throw new Exception(name + " must be inside the current display");
        }
        return value;
    }

    private static JSONObject error(String message) {
        try {
            return new JSONObject().put("error", message == null ? "unknown_error" : message);
        } catch (Exception impossible) {
            return new JSONObject();
        }
    }

    private static String readLine(BufferedInputStream input) throws Exception {
        ByteArrayOutputStream line = new ByteArrayOutputStream();
        int previous = -1;
        while (line.size() <= 8192) {
            int value = input.read();
            if (value < 0) {
                return line.size() == 0 ? null : line.toString("UTF-8");
            }
            if (previous == '\r' && value == '\n') {
                byte[] bytes = line.toByteArray();
                return new String(bytes, 0, Math.max(0, bytes.length - 1), StandardCharsets.UTF_8);
            }
            line.write(value);
            previous = value;
        }
        throw new Exception("HTTP header line is too long");
    }

    private static byte[] readBytes(BufferedInputStream input, int length) throws Exception {
        byte[] result = new byte[length];
        int offset = 0;
        while (offset < length) {
            int count = input.read(result, offset, length - offset);
            if (count < 0) {
                throw new EOFException("request body ended early");
            }
            offset += count;
        }
        return result;
    }

    private static void respond(BufferedOutputStream output, int status, JSONObject body) throws Exception {
        byte[] encoded = body.toString().getBytes(StandardCharsets.UTF_8);
        String reason = status == 200 ? "OK" : status == 400 ? "Bad Request"
                : status == 401 ? "Unauthorized" : status == 404 ? "Not Found"
                : status == 409 ? "Conflict" : "Payload Too Large";
        String headers = "HTTP/1.1 " + status + " " + reason + "\r\n"
                + "Content-Type: application/json\r\n"
                + "Content-Length: " + encoded.length + "\r\n"
                + "Connection: close\r\n\r\n";
        output.write(headers.getBytes(StandardCharsets.US_ASCII));
        output.write(encoded);
        output.flush();
    }

    private static final class PermissionException extends Exception {
        PermissionException(String message) {
            super(message);
        }
    }
}
