package com.ssine.codexnode;

import android.Manifest;
import android.annotation.SuppressLint;
import android.app.Activity;
import android.content.Intent;
import android.content.pm.PackageManager;
import android.net.Uri;
import android.os.Build;
import android.os.Bundle;
import android.os.Handler;
import android.os.Looper;
import android.graphics.Color;
import android.view.Gravity;
import android.view.View;
import android.view.WindowInsets;
import android.view.WindowInsetsController;
import android.view.WindowManager;
import android.webkit.CookieManager;
import android.webkit.ValueCallback;
import android.webkit.WebChromeClient;
import android.webkit.WebResourceRequest;
import android.webkit.WebResourceError;
import android.webkit.WebResourceResponse;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.webkit.WebViewClient;
import android.widget.Button;
import android.widget.FrameLayout;
import android.widget.LinearLayout;
import android.widget.TextView;
import androidx.webkit.JavaScriptReplyProxy;
import androidx.webkit.WebViewCompat;
import androidx.webkit.WebViewFeature;
import org.json.JSONObject;
import java.net.URI;
import java.net.HttpURLConnection;
import java.net.URL;
import java.io.InputStream;
import java.io.OutputStream;
import java.util.Collections;

public final class ConsoleActivity extends Activity {
    private static final int FILES = 201, NOTIFICATIONS = 202, SAVE_FILE = 203;
    private WebView web;
    private TextView notice;
    private FrameLayout layout;
    private LinearLayout feedback, recovery;
    private Button settingsFallback;
    private boolean shellIntegrated, pageFailed;
    private int insetTop;
    private String server = "", origin = "", pendingThread;
    private ValueCallback<Uri[]> fileCallback;
    private JavaScriptReplyProxy permissionReply;
    private String permissionID;
    private String downloadURL;
    private volatile boolean cancelDownload;
    private volatile HttpURLConnection activeDownload;
    private final Handler handler = new Handler(Looper.getMainLooper());
    private boolean ready;
    private ConsoleDownloads blobDownloads;

    static String originOf(String value) {
        try {
            URI uri = new URI(value);
            if (!"https".equals(uri.getScheme()) && !(BuildConfig.DEBUG && "http".equals(uri.getScheme()))) return "";
            if (uri.getHost() == null || uri.getRawUserInfo() != null) return "";
            int port = uri.getPort();
            return uri.getScheme() + "://" + uri.getHost().toLowerCase(java.util.Locale.ROOT)
                    + (port < 0 || port == (uri.getScheme().equals("https") ? 443 : 80) ? "" : ":" + port);
        } catch (Exception error) { return ""; }
    }
    private boolean trusted(String url) { return !origin.isEmpty() && origin.equals(originOf(url)); }
    @Override public void onCreate(Bundle state) {
        super.onCreate(state);
        layout = new FrameLayout(this);
        web = new WebView(this); layout.addView(web, new FrameLayout.LayoutParams(-1, -1));
        feedback = new LinearLayout(this); feedback.setOrientation(LinearLayout.VERTICAL);
        feedback.setPadding(dp(20), dp(16), dp(20), dp(16)); feedback.setVisibility(View.GONE);
        notice = new TextView(this); notice.setTextSize(15); feedback.addView(notice);
        recovery = new LinearLayout(this); recovery.setVisibility(View.GONE);
        Button reload = new Button(this); reload.setText("重试连接"); reload.setOnClickListener(v -> {
            if (origin.isEmpty()) { openSettings(); return; }
            message(""); web.loadUrl(server + "/?launch=pwa");
        });
        Button settings = new Button(this); settings.setText("设备设置"); settings.setOnClickListener(v -> openSettings());
        recovery.addView(reload); recovery.addView(settings); feedback.addView(recovery);
        FrameLayout.LayoutParams feedbackParams = new FrameLayout.LayoutParams(-1, -2, Gravity.TOP);
        layout.addView(feedback, feedbackParams);
        // Older Servers have no settings bridge. Keep one compact escape hatch until
        // the page acknowledges the integrated shell, including when its JS fails.
        settingsFallback = new Button(this); settingsFallback.setText("⋯");
        settingsFallback.setContentDescription("设备设置"); settingsFallback.setVisibility(View.GONE);
        settingsFallback.setOnClickListener(v -> openSettings());
        layout.addView(settingsFallback, new FrameLayout.LayoutParams(dp(48), dp(48), Gravity.TOP | Gravity.END));
        setContentView(layout); configureWindow();
        blobDownloads = new ConsoleDownloads(this, text -> { message(text); notice.setOnClickListener(v -> { blobDownloads.cancel(); message("下载已取消"); }); });
        configure(); acceptIntent(getIntent()); loadServer(state);
    }
    private int dp(int value) { return Math.round(value * getResources().getDisplayMetrics().density); }
    private void openSettings() { startActivity(new Intent(this, MainActivity.class)); }
    private void message(String text) {
        notice.setOnClickListener(null); notice.setText(text);
        feedback.setVisibility(text.isEmpty() ? View.GONE : View.VISIBLE);
        recovery.setVisibility(View.GONE);
    }
    private void connectionFailure(String text) {
        pageFailed = true; message(text); recovery.setVisibility(View.VISIBLE);
        web.setVisibility(View.INVISIBLE);
        settingsFallback.setVisibility(View.GONE);
    }
    @SuppressWarnings("deprecation") private void configureWindow() {
        if (Build.VERSION.SDK_INT >= 30) getWindow().setDecorFitsSystemWindows(false);
        else getWindow().getDecorView().setSystemUiVisibility(View.SYSTEM_UI_FLAG_LAYOUT_STABLE
                | View.SYSTEM_UI_FLAG_LAYOUT_FULLSCREEN | View.SYSTEM_UI_FLAG_LAYOUT_HIDE_NAVIGATION);
        getWindow().setStatusBarColor(Color.TRANSPARENT);
        getWindow().setNavigationBarColor(Color.TRANSPARENT);
        if (Build.VERSION.SDK_INT >= 28) {
            WindowManager.LayoutParams attributes = getWindow().getAttributes();
            attributes.layoutInDisplayCutoutMode = WindowManager.LayoutParams.LAYOUT_IN_DISPLAY_CUTOUT_MODE_SHORT_EDGES;
            getWindow().setAttributes(attributes);
        }
        if (Build.VERSION.SDK_INT >= 29) {
            getWindow().setStatusBarContrastEnforced(false);
            getWindow().setNavigationBarContrastEnforced(false);
        }
        layout.setOnApplyWindowInsetsListener((view, insets) -> {
            int left, right, bottom;
            if (Build.VERSION.SDK_INT >= 30) {
                android.graphics.Insets bars = insets.getInsets(WindowInsets.Type.systemBars() | WindowInsets.Type.displayCutout());
                insetTop = bars.top; left = bars.left; right = bars.right;
                bottom = Math.max(bars.bottom, insets.getInsets(WindowInsets.Type.ime()).bottom);
            } else {
                insetTop = insets.getSystemWindowInsetTop(); left = insets.getSystemWindowInsetLeft();
                right = insets.getSystemWindowInsetRight(); bottom = insets.getSystemWindowInsetBottom();
            }
            // The reading surface extends under the status bar. Native padding still
            // protects side cutouts, navigation gestures and the resized IME viewport.
            view.setPadding(left, shellIntegrated ? 0 : insetTop, right, bottom);
            FrameLayout.LayoutParams params = (FrameLayout.LayoutParams) feedback.getLayoutParams();
            params.topMargin = shellIntegrated ? insetTop : 0; feedback.setLayoutParams(params);
            if (shellIntegrated && trusted(web.getUrl())) web.evaluateJavascript(
                    "window.dispatchEvent(new CustomEvent('mira:native-insets',{detail:" + windowState() + "}))", null);
            return Build.VERSION.SDK_INT >= 30 ? WindowInsets.CONSUMED : insets.consumeSystemWindowInsets();
        });
        applyAppearance(false); layout.requestApplyInsets();
    }
    private JSONObject windowState() {
        JSONObject result = new JSONObject();
        try { result.put("statusBarHeight", insetTop / getResources().getDisplayMetrics().density); result.put("integrated", true); }
        catch (Exception ignored) { }
        return result;
    }
    @SuppressWarnings("deprecation") private void applyAppearance(boolean dark) {
        int background = Color.parseColor(dark ? "#1f2226" : "#ffffff");
        layout.setBackgroundColor(background); web.setBackgroundColor(background);
        feedback.setBackgroundColor(background); notice.setTextColor(Color.parseColor(dark ? "#e9edf2" : "#323130"));
        if (Build.VERSION.SDK_INT >= 30) {
            WindowInsetsController controller = getWindow().getInsetsController();
            if (controller != null) {
                int light = WindowInsetsController.APPEARANCE_LIGHT_STATUS_BARS | WindowInsetsController.APPEARANCE_LIGHT_NAVIGATION_BARS;
                controller.setSystemBarsAppearance(dark ? 0 : light, light);
            }
        } else {
            View decor = getWindow().getDecorView();
            int light = View.SYSTEM_UI_FLAG_LIGHT_STATUS_BAR | View.SYSTEM_UI_FLAG_LIGHT_NAVIGATION_BAR;
            decor.setSystemUiVisibility(dark ? decor.getSystemUiVisibility() & ~light : decor.getSystemUiVisibility() | light);
        }
    }
    @SuppressLint("SetJavaScriptEnabled") private void configure() {
        WebSettings settings = web.getSettings(); settings.setJavaScriptEnabled(true); settings.setDomStorageEnabled(true);
        settings.setAllowFileAccess(false); settings.setAllowContentAccess(true); settings.setMixedContentMode(WebSettings.MIXED_CONTENT_NEVER_ALLOW);
        settings.setJavaScriptCanOpenWindowsAutomatically(false); settings.setSupportMultipleWindows(false);
        CookieManager.getInstance().setAcceptCookie(true); CookieManager.getInstance().setAcceptThirdPartyCookies(web, false);
        WebView.setWebContentsDebuggingEnabled(BuildConfig.DEBUG);
        web.setWebViewClient(new WebViewClient() {
            @Override public boolean shouldOverrideUrlLoading(WebView view, WebResourceRequest request) {
                if (trusted(request.getUrl().toString())) return false;
                if (request.isForMainFrame() && request.hasGesture()) external(request.getUrl());
                return true;
            }
            @Override public void onPageStarted(WebView view, String url, android.graphics.Bitmap icon) {
                ready = false; pageFailed = false; shellIntegrated = false; message(""); web.setVisibility(View.VISIBLE);
                settingsFallback.setVisibility(View.GONE); layout.requestApplyInsets();
            }
            @Override public void onPageFinished(WebView view, String url) {
                if (trusted(url)) { CookieManager.getInstance().flush(); }
                if (!shellIntegrated && !pageFailed) settingsFallback.setVisibility(View.VISIBLE);
            }
            @Override public void onReceivedError(WebView view, WebResourceRequest request, WebResourceError error) {
                if (request.isForMainFrame()) connectionFailure("暂时无法连接 Mira，请检查网络后重试。");
            }
            @Override public void onReceivedHttpError(WebView view, WebResourceRequest request, WebResourceResponse response) {
                if (request.isForMainFrame()) connectionFailure("Mira 暂时不可用，请稍后重试。");
            }
            @Override public void onReceivedSslError(WebView view, android.webkit.SslErrorHandler callback, android.net.http.SslError error) {
                callback.cancel(); connectionFailure("无法安全连接 Mira，请检查 Server 的 HTTPS 证书。");
            }
            // TLS failures retain WebView's default cancellation behavior.
        });
        web.setWebChromeClient(new WebChromeClient() {
            @Override public boolean onShowFileChooser(WebView view, ValueCallback<Uri[]> callback, FileChooserParams params) {
                if (!trusted(view.getUrl())) return false;
                if (fileCallback != null) fileCallback.onReceiveValue(null);
                fileCallback = callback;
                try { startActivityForResult(params.createIntent(), FILES); }
                catch (RuntimeException error) { fileCallback.onReceiveValue(null); fileCallback = null; message("无法打开文件选择器"); }
                return true;
            }
        });
        web.setDownloadListener((url, userAgent, disposition, mime, length) -> {
            if (!trusted(url)) { message("请在浏览器中打开此下载链接"); return; }
            if (downloadURL != null || activeDownload != null) { message("请先完成或取消当前下载"); return; }
            downloadURL = url;
            Intent intent = new Intent(Intent.ACTION_CREATE_DOCUMENT).addCategory(Intent.CATEGORY_OPENABLE)
                    .setType(mime == null || mime.isEmpty() ? "application/octet-stream" : mime)
                    .putExtra(Intent.EXTRA_TITLE, android.webkit.URLUtil.guessFileName(url, disposition, mime));
            try { startActivityForResult(intent, SAVE_FILE); } catch (RuntimeException error) { downloadURL = null; message("无法打开保存文件窗口"); }
        });
    }
    private void external(Uri uri) {
        if (!"https".equals(uri.getScheme()) && !"http".equals(uri.getScheme()) && !"mailto".equals(uri.getScheme())) return;
        try { startActivity(new Intent(Intent.ACTION_VIEW, uri).addCategory(Intent.CATEGORY_BROWSABLE)); }
        catch (RuntimeException error) { message("没有可打开此链接的应用"); }
    }
    private void loadServer(Bundle state) {
        NodeConfig config = NodeConfig.load(this); server = config.serverUrl; origin = originOf(server);
        if (origin.isEmpty() || !server.replaceAll("/+$", "").equals(origin)) {
            connectionFailure("请在设备设置中填写 Mira Server 的 HTTPS 地址。"); return;
        }
        if (!WebViewFeature.isFeatureSupported(WebViewFeature.WEB_MESSAGE_LISTENER)) {
            message("请更新 Android System WebView，以使用应用内通知。");
        } else {
            WebViewCompat.addWebMessageListener(web, "MiraAndroid", Collections.singleton(origin), (view, data, source, main, reply) -> {
                if (!main || !origin.equals(originOf(source.toString())) || data.getData() == null || data.getData().length() > 70000) return;
                try { handle(new JSONObject(data.getData()), reply); } catch (Exception error) { /* Malformed page messages have no native effects. */ }
            });
        }
        if (pendingThread != null) web.loadUrl(server + "/?thread=" + pendingThread);
        else if (state == null || web.restoreState(state) == null || !trusted(web.getUrl())) web.loadUrl(server + "/?launch=pwa");
    }
    private void handle(JSONObject request, JavaScriptReplyProxy reply) throws Exception {
        String id = request.optString("id"), method = request.optString("method");
        if (!id.matches("[0-9]{1,10}")) return;
        if (blobDownloads.handle(request, reply)) return;
        switch (method) {
            case "shell":
                shellIntegrated = true; settingsFallback.setVisibility(View.GONE);
                applyAppearance(request.optBoolean("dark")); layout.requestApplyInsets();
                respond(reply, id, windowState(), null); break;
            case "settings": openSettings(); respond(reply, id, new JSONObject(), null); break;
            case "context": respond(reply, id, CompletionNotifications.state(this), null); break;
            case "permission":
                CompletionNotifications.channels(this);
                if (Build.VERSION.SDK_INT >= 33 && checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED) {
                    if (permissionReply != null) { respond(reply, id, null, "已有权限请求正在进行"); return; }
                    permissionReply = reply; permissionID = id; requestPermissions(new String[]{Manifest.permission.POST_NOTIFICATIONS}, NOTIFICATIONS);
                } else respond(reply, id, CompletionNotifications.state(this), null);
                break;
            case "setEnabled":
                try { CompletionNotifications.setEnabled(this, request.getBoolean("enabled")); respond(reply, id, CompletionNotifications.state(this), null); }
                catch (Exception error) { respond(reply, id, null, error.getMessage()); }
                break;
            case "ready": ready = true; respond(reply, id, new JSONObject(), null); openPending(); break;
            case "navigation":
                if (request.optString("threadId").equals(pendingThread) && request.optBoolean("accepted")) { pendingThread = null; message(""); }
                else if (pendingThread != null) {
                    message("对话已完成。当前操作结束后，点此打开。"); notice.setOnClickListener(v -> openPending());
                }
                respond(reply, id, new JSONObject(), null); break;
        }
    }
    private void respond(JavaScriptReplyProxy reply, String id, JSONObject result, String error) {
        try { JSONObject value = new JSONObject().put("id", id); if (error == null) value.put("result", result); else value.put("error", error); reply.postMessage(value.toString()); }
        catch (Exception ignored) { }
    }
    private void acceptIntent(Intent intent) {
        String thread = intent.getStringExtra("threadId");
        if (CompletionNotifications.uuid(thread) && NodeConfig.load(this).serverUrl.equals(intent.getStringExtra("serverUrl"))) pendingThread = thread;
    }
    private void openPending() {
        if (!ready || pendingThread == null || !trusted(web.getUrl())) return;
        String script = "window.dispatchEvent(new CustomEvent('mira:native-notification',{detail:" + JSONObject.quote(pendingThread) + "}))";
        web.evaluateJavascript(script, null);
    }
    @Override protected void onNewIntent(Intent intent) { super.onNewIntent(intent); setIntent(intent); acceptIntent(intent); openPending(); }
    @Override protected void onResume() {
        super.onResume();
        NodeConfig config = NodeConfig.load(this);
        if (config.autoStart && config.validate() == null && !NodeConfig.isUserStopped(this) && !MiraNodeService.isRunning()) MiraNodeService.start(this);
        if (!server.equals(config.serverUrl)) { recreate(); return; }
        if (web != null) { web.onResume(); openPending(); }
    }
    @Override protected void onPause() { if (web != null) { CookieManager.getInstance().flush(); web.onPause(); } super.onPause(); }
    @Override protected void onSaveInstanceState(Bundle state) { super.onSaveInstanceState(state); web.saveState(state); }
    @Override public void onBackPressed() {
        if (web.canGoBack()) web.goBack(); else super.onBackPressed();
    }
    @Override public void onRequestPermissionsResult(int code, String[] permissions, int[] grants) {
        super.onRequestPermissionsResult(code, permissions, grants);
        if (code == NOTIFICATIONS && permissionReply != null) {
            try { respond(permissionReply, permissionID, CompletionNotifications.state(this), null); } catch (Exception ignored) { }
            permissionReply = null;
        }
    }
    @Override protected void onActivityResult(int code, int result, Intent data) {
        super.onActivityResult(code, result, data);
        if (code == ConsoleDownloads.SAVE_BLOB) blobDownloads.onResult(result, data);
        if (code == FILES && fileCallback != null) { fileCallback.onReceiveValue(WebChromeClient.FileChooserParams.parseResult(result, data)); fileCallback = null; }
        if (code == SAVE_FILE) {
            String url = downloadURL; downloadURL = null;
            if (result == RESULT_OK && data != null && data.getData() != null && url != null) download(url, data.getData());
        }
    }
    private void download(String url, Uri destination) {
        String cookies = CookieManager.getInstance().getCookie(url), agent = web.getSettings().getUserAgentString();
        cancelDownload = false; message("正在下载，点此取消"); notice.setOnClickListener(v -> { cancelDownload = true; HttpURLConnection c = activeDownload; if (c != null) c.disconnect(); });
        new Thread(() -> {
            try {
                String next = url; HttpURLConnection connection = null;
                for (int count = 0; count < 5; count++) {
                    if (cancelDownload || !trusted(next)) throw new Exception("下载已取消或链接无效");
                    connection = (HttpURLConnection)new URL(next).openConnection(); activeDownload = connection;
                    connection.setConnectTimeout(15000); connection.setReadTimeout(30000); connection.setInstanceFollowRedirects(false);
                    connection.setRequestProperty("User-Agent", agent); if (cookies != null) connection.setRequestProperty("Cookie", cookies);
                    int status = connection.getResponseCode();
                    if (status >= 300 && status < 400) { next = new URL(new URL(next), connection.getHeaderField("Location")).toString(); connection.disconnect(); continue; }
                    if (status != 200) throw new Exception("下载失败，请确认登录状态"); break;
                }
                if (connection == null || connection.getResponseCode() != 200) throw new Exception("下载重定向过多");
                long total = connection.getContentLengthLong(), copied = 0, lastUpdate = 0;
                try (InputStream input = connection.getInputStream(); OutputStream output = getContentResolver().openOutputStream(destination, "w")) {
                    byte[] buffer = new byte[65536]; int size;
                    while ((size = input.read(buffer)) >= 0) {
                        if (cancelDownload) throw new Exception("下载已取消"); output.write(buffer, 0, size); copied += size;
                        if (System.currentTimeMillis() - lastUpdate > 500) { lastUpdate = System.currentTimeMillis(); String progress = "已下载 " + copied / 1024 + " KB" + (total > 0 ? " / " + total / 1024 + " KB" : "") + "，点此取消"; runOnUiThread(() -> message(progress)); }
                    }
                }
                runOnUiThread(() -> message("下载完成"));
            } catch (Exception error) {
                runOnUiThread(() -> message(cancelDownload ? "下载已取消" : "下载失败，请重试"));
                try { android.provider.DocumentsContract.deleteDocument(getContentResolver(), destination); } catch (Exception ignored) { }
            } finally {
                HttpURLConnection connection = activeDownload; if (connection != null) connection.disconnect(); activeDownload = null;
                runOnUiThread(() -> notice.setOnClickListener(null));
            }
        }, "mira-download").start();
    }
    @Override protected void onDestroy() {
        cancelDownload = true; HttpURLConnection connection = activeDownload; if (connection != null) connection.disconnect();
        if (fileCallback != null) fileCallback.onReceiveValue(null);
        blobDownloads.close();
        web.destroy(); handler.removeCallbacksAndMessages(null); super.onDestroy();
    }
}
