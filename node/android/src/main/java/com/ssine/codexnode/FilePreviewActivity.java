package com.ssine.codexnode;

import android.annotation.SuppressLint;
import android.app.Activity;
import android.content.Intent;
import android.net.Uri;
import android.os.Bundle;
import android.os.Message;
import android.webkit.CookieManager;
import android.webkit.WebChromeClient;
import android.webkit.WebResourceRequest;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.webkit.WebViewClient;
import android.widget.LinearLayout;
import android.widget.Button;
import android.widget.FrameLayout;
import android.widget.TextView;
import android.view.View;
import android.view.WindowManager;
import java.net.HttpURLConnection;
import java.net.URL;
import java.io.InputStream;
import java.io.OutputStream;

/** Separate browser surface. It deliberately installs no MiraAndroid bridge. */
public final class FilePreviewActivity extends Activity {
    private WebView web;
    private String initial, downloadURL;
    private FrameLayout frame;
    private View customView;
    private WebChromeClient.CustomViewCallback customCallback;
    private TextView status;
    private volatile HttpURLConnection download;
    private volatile boolean canceled;
    @SuppressLint("SetJavaScriptEnabled") @Override public void onCreate(Bundle state) {
        super.onCreate(state);
        initial = getIntent().getStringExtra("url");
        if (ConsoleActivity.originOf(initial).isEmpty()) { finish(); return; }
        LinearLayout layout = new LinearLayout(this); layout.setOrientation(LinearLayout.VERTICAL);
        Button back = new Button(this); back.setText("返回对话"); back.setOnClickListener(v -> finish()); layout.addView(back);
        web = new WebView(this); layout.addView(web, new LinearLayout.LayoutParams(-1, 0, 1)); frame = new FrameLayout(this); frame.addView(layout); setContentView(frame);
        status = new TextView(this); layout.addView(status, 1);
        status.setOnClickListener(v -> { canceled = true; if (download != null) download.disconnect(); status.setText("下载已取消"); });
        WebSettings settings = web.getSettings(); settings.setJavaScriptEnabled(true); settings.setDomStorageEnabled(true);
        settings.setAllowFileAccess(false); settings.setAllowContentAccess(false); settings.setMixedContentMode(WebSettings.MIXED_CONTENT_NEVER_ALLOW);
        settings.setSupportMultipleWindows(true); settings.setJavaScriptCanOpenWindowsAutomatically(false);
        CookieManager.getInstance().setAcceptCookie(true); CookieManager.getInstance().setAcceptThirdPartyCookies(web, false);
        web.setWebViewClient(new WebViewClient() {
            @Override public boolean shouldOverrideUrlLoading(WebView view, WebResourceRequest request) {
                return ConsoleActivity.originOf(request.getUrl().toString()).isEmpty();
            }
        });
        web.setWebChromeClient(new WebChromeClient() {
            @Override public boolean onCreateWindow(WebView view, boolean dialog, boolean gesture, Message result) {
                if (!gesture) return false;
                return createWindow(FilePreviewActivity.this, result, null);
            }
            @Override public void onCloseWindow(WebView view) { if (view == web) finish(); }
            @Override public void onShowCustomView(View view, CustomViewCallback callback) {
                if (customView != null) { callback.onCustomViewHidden(); return; }
                customView = view; customCallback = callback; web.setVisibility(View.GONE);
                frame.addView(view, new FrameLayout.LayoutParams(-1, -1)); getWindow().addFlags(WindowManager.LayoutParams.FLAG_FULLSCREEN);
            }
            @Override public void onHideCustomView() { hideCustomView(); }
        });
        web.setDownloadListener((url, agent, disposition, mime, length) -> {
            if (downloadURL != null || download != null || ConsoleActivity.originOf(url).isEmpty()) return;
            downloadURL = url;
            Intent intent = new Intent(Intent.ACTION_CREATE_DOCUMENT).addCategory(Intent.CATEGORY_OPENABLE)
                    .setType(mime == null ? "application/octet-stream" : mime)
                    .putExtra(Intent.EXTRA_TITLE, android.webkit.URLUtil.guessFileName(url, disposition, mime));
            try { startActivityForResult(intent, 301); } catch (RuntimeException error) { downloadURL = null; status.setText("无法打开保存窗口"); }
        });
        if (state != null) web.restoreState(state); else web.loadUrl(initial);
    }
    // The first window is admitted only from the console to its file page.
    // Nested file-page windows run in another bridge-free Activity.
    static boolean createWindow(Activity owner, Message result, String consoleOrigin) {
        WebView pending = new WebView(owner);
        pending.setWebViewClient(new WebViewClient() {
            private boolean opened;
            { pending.postDelayed(() -> { if (!opened) { opened = true; pending.destroy(); } }, 45000); }
            private boolean open(String url) {
                if (opened || "about:blank".equals(url)) return true;
                opened = true;
                boolean safe = !ConsoleActivity.originOf(url).isEmpty();
                if (consoleOrigin != null) safe = safe && consoleOrigin.equals(ConsoleActivity.originOf(url)) && "/files.html".equals(Uri.parse(url).getPath());
                if (safe) owner.startActivity(new Intent(owner, FilePreviewActivity.class).putExtra("url", url));
                else if (consoleOrigin != null && (url.startsWith("https:") || url.startsWith("http:"))) {
                    try { owner.startActivity(new Intent(Intent.ACTION_VIEW, Uri.parse(url))); } catch (RuntimeException ignored) { }
                }
                pending.stopLoading(); pending.post(pending::destroy); return true;
            }
            @Override public boolean shouldOverrideUrlLoading(WebView view, WebResourceRequest request) { return open(request.getUrl().toString()); }
            @Override public void onPageStarted(WebView view, String url, android.graphics.Bitmap icon) { open(url); }
        });
        WebView.WebViewTransport transport = (WebView.WebViewTransport) result.obj; transport.setWebView(pending); result.sendToTarget(); return true;
    }
    private void hideCustomView() {
        if (customView == null) return;
        frame.removeView(customView); customView = null; web.setVisibility(View.VISIBLE);
        getWindow().clearFlags(WindowManager.LayoutParams.FLAG_FULLSCREEN);
        if (customCallback != null) customCallback.onCustomViewHidden(); customCallback = null;
    }
    @Override protected void onActivityResult(int request, int result, Intent data) {
        super.onActivityResult(request, result, data);
        if (request != 301) return;
        String url = downloadURL; downloadURL = null;
        if (result != RESULT_OK || data == null || data.getData() == null || url == null) return;
        Uri destination = data.getData(); canceled = false;
        new Thread(() -> {
            HttpURLConnection connection = null;
            try {
                connection = (HttpURLConnection) new URL(url).openConnection(); download = connection;
                connection.setConnectTimeout(15000); connection.setReadTimeout(30000); connection.setInstanceFollowRedirects(false);
                String cookie = CookieManager.getInstance().getCookie(url); if (cookie != null) connection.setRequestProperty("Cookie", cookie);
                if (connection.getResponseCode() != 200) throw new java.io.IOException("HTTP " + connection.getResponseCode());
                try (InputStream input = connection.getInputStream(); OutputStream output = getContentResolver().openOutputStream(destination)) {
                    if (output == null) throw new java.io.IOException("无法写入文件");
                    byte[] buffer = new byte[65536]; long count = 0, next = 0; int size;
                    while (!canceled && (size = input.read(buffer)) != -1) {
                        output.write(buffer, 0, size); count += size;
                        if (count >= next) { next = count + 1048576; final long progress = count;
                            runOnUiThread(() -> status.setText("已下载 " + progress + " 字节 · 点击取消")); }
                    }
                }
                runOnUiThread(() -> status.setText(canceled ? "下载已取消" : "下载完成"));
            } catch (Exception error) { runOnUiThread(() -> status.setText(canceled ? "下载已取消" : "下载失败，可以重试")); }
            finally { if (connection != null) connection.disconnect(); download = null; }
        }, "mira-preview-download").start();
    }
    @Override protected void onSaveInstanceState(Bundle state) { web.saveState(state); super.onSaveInstanceState(state); }
    @Override protected void onPause() { if (web != null) web.onPause(); super.onPause(); }
    @Override protected void onResume() { super.onResume(); if (web != null) web.onResume(); }
    @Override protected void onDestroy() { canceled = true; if (download != null) download.disconnect(); hideCustomView(); if (web != null) web.destroy(); super.onDestroy(); }
    @Override public void onBackPressed() { if (customView != null) hideCustomView(); else if (web != null && web.canGoBack()) web.goBack(); else super.onBackPressed(); }
}
