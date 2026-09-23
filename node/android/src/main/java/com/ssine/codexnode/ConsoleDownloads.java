package com.ssine.codexnode;

import android.app.Activity;
import android.content.Intent;
import android.net.Uri;
import android.util.Base64;
import androidx.webkit.JavaScriptReplyProxy;
import org.json.JSONObject;
import java.io.OutputStream;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.function.Consumer;

// Blob downloads cross the origin-scoped bridge one acknowledged 48 KiB chunk
// at a time. The document picker grants access to exactly the chosen destination.
final class ConsoleDownloads {
    static final int SAVE_BLOB = 204;
    private final Activity activity;
    private final Consumer<String> status;
    private final ExecutorService io = Executors.newSingleThreadExecutor();
    private JavaScriptReplyProxy picker;
    private String pickerID;
    private volatile OutputStream output;
    private volatile boolean busy, closed;
    private Uri destination;
    private long bytes;
    ConsoleDownloads(Activity activity, Consumer<String> status) { this.activity = activity; this.status = status; }
    private void reply(JavaScriptReplyProxy proxy, String id, String error) {
        activity.runOnUiThread(() -> { try { proxy.postMessage(new JSONObject().put("id", id)
                .put(error == null ? "result" : "error", error == null ? new JSONObject() : error).toString()); } catch (Exception ignored) { } });
    }
    boolean handle(JSONObject request, JavaScriptReplyProxy proxy) throws Exception {
        String method = request.optString("method"), id = request.optString("id");
        if (!method.startsWith("download")) return false;
        if (closed) { reply(proxy, id, "下载窗口已关闭"); return true; }
        if (method.equals("downloadStart")) {
            if (picker != null || output != null || busy) { reply(proxy, id, "已有下载正在进行"); return true; }
            String name = request.optString("name", "download").replaceAll("[/\\\\]", "_");
            if (name.length() > 255) name = name.substring(0, 255);
            picker = proxy; pickerID = id;
            try { activity.startActivityForResult(new Intent(Intent.ACTION_CREATE_DOCUMENT).addCategory(Intent.CATEGORY_OPENABLE)
                    .setType("application/octet-stream").putExtra(Intent.EXTRA_TITLE, name), SAVE_BLOB); }
            catch (RuntimeException error) { picker = null; reply(proxy, id, "无法打开保存文件窗口"); }
            return true;
        }
        if (method.equals("downloadCancel")) { cancel(); reply(proxy, id, null); return true; }
        if (output == null || busy) { reply(proxy, id, "下载尚未就绪"); return true; }
        if (method.equals("downloadChunk")) {
            String encoded = request.optString("data");
            if (encoded.length() > 65536) { reply(proxy, id, "下载数据块过大"); return true; }
            byte[] chunk = Base64.decode(encoded, Base64.NO_WRAP);
            if (chunk.length > 49152) { reply(proxy, id, "下载数据块过大"); return true; }
            busy = true;
            io.execute(() -> {
                try { OutputStream target = output; if (target == null) throw new Exception(); target.write(chunk); bytes += chunk.length;
                    activity.runOnUiThread(() -> status.accept("已下载 " + bytes / 1024 + " KB，点此取消")); reply(proxy, id, null);
                } catch (Exception error) { cancel(); reply(proxy, id, "下载已取消或写入失败"); }
                finally { busy = false; }
            });
        } else if (method.equals("downloadFinish")) {
            busy = true;
            io.execute(() -> { try { OutputStream target = output; output = null; if (target != null) target.close(); destination = null;
                    activity.runOnUiThread(() -> status.accept("下载完成")); reply(proxy, id, null);
                } catch (Exception error) { cancel(); reply(proxy, id, "无法完成下载"); } finally { busy = false; }
            });
        } else reply(proxy, id, "未知的下载操作");
        return true;
    }
    void onResult(int result, Intent data) {
        JavaScriptReplyProxy proxy = picker; String id = pickerID; picker = null;
        if (proxy == null) return;
        if (result != Activity.RESULT_OK || data == null || data.getData() == null) { reply(proxy, id, "下载已取消"); return; }
        destination = data.getData(); busy = true;
        io.execute(() -> { try {
                if (closed) throw new Exception(); output = activity.getContentResolver().openOutputStream(destination, "w"); bytes = 0;
                if (output == null) throw new Exception(); activity.runOnUiThread(() -> status.accept("正在下载，点此取消")); reply(proxy, id, null);
            } catch (Exception error) { cancel(); reply(proxy, id, "无法创建下载文件"); } finally { busy = false; }
        });
    }
    void cancel() {
        OutputStream target = output; output = null;
        try { if (target != null) target.close(); } catch (Exception ignored) { }
        Uri uri = destination; destination = null;
        if (uri != null) try { android.provider.DocumentsContract.deleteDocument(activity.getContentResolver(), uri); } catch (Exception ignored) { }
    }
    void close() { closed = true; cancel(); io.shutdownNow(); }
}
