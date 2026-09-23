package com.ssine.codexnode;

import android.Manifest;
import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.content.Context;
import android.content.Intent;
import android.content.SharedPreferences;
import android.content.pm.PackageManager;
import android.net.Uri;
import android.os.Build;
import org.json.JSONObject;
import java.util.ArrayList;
import java.util.Comparator;

final class CompletionNotifications {
    static final String CHANNEL = "mira_completions";
    private static final String PREFS = "mira_completion_notifications";
    static boolean uuid(String value) { return value != null && value.matches("(?i)[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}"); }
    private static SharedPreferences prefs(Context context) { return context.getSharedPreferences(PREFS, Context.MODE_PRIVATE); }
    private static String scope(Context context) { NodeConfig c = NodeConfig.load(context); return c.serverUrl + "|" + c.nodeKey; }
    static boolean enabled(Context context) { return prefs(context).getBoolean("enabled:" + scope(context), false); }
    static boolean permitted(Context context) {
        NotificationManager manager = context.getSystemService(NotificationManager.class);
        NotificationChannel channel = manager.getNotificationChannel(CHANNEL);
        return (Build.VERSION.SDK_INT < 33 || context.checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) == PackageManager.PERMISSION_GRANTED)
                && manager.areNotificationsEnabled() && (channel == null || channel.getImportance() != NotificationManager.IMPORTANCE_NONE);
    }
    static void channels(Context context) {
        NotificationChannel channel = new NotificationChannel(CHANNEL, "对话完成", NotificationManager.IMPORTANCE_DEFAULT);
        channel.setDescription("Mira 对话完成后的提醒");
        context.getSystemService(NotificationManager.class).createNotificationChannel(channel);
    }
    static synchronized void setEnabled(Context context, boolean value) throws Exception {
        if (value && !permitted(context)) throw new Exception("请在 Android 设置中允许 Mira 通知");
        if (!prefs(context).edit().putBoolean("enabled:" + scope(context), value).commit()) throw new Exception("无法保存通知设置");
        if (!value) {
            NotificationManager manager = context.getSystemService(NotificationManager.class);
            for (android.service.notification.StatusBarNotification item : manager.getActiveNotifications()) {
                if (item.getTag() != null && item.getTag().startsWith("completion:")) manager.cancel(item.getTag(), item.getId());
            }
        }
    }
    static JSONObject state(Context context) throws Exception {
        return new JSONObject().put("nodeKey", NodeConfig.load(context).nodeKey)
                .put("enabled", enabled(context)).put("permission", permitted(context) ? "granted" : "denied");
    }
    static synchronized JSONObject deliver(Context context, JSONObject value) throws Exception {
        String id = value.optString("deliveryId"), thread = value.optString("threadId"), title = value.optString("title");
        long expiry = value.optLong("expiresAt"), now = System.currentTimeMillis();
        if (!uuid(id) || !uuid(thread) || title.length() > 256 || expiry > now + 86460000L) throw new Exception("invalid_completion");
        if (!NodeConfig.load(context).serverUrl.equals(value.optString("serverUrl"))) throw new Exception("server_changed");
        if (expiry <= now || !enabled(context)) return new JSONObject().put("accepted", true).put("suppressed", true);
        if (!permitted(context)) throw new Exception("notification_permission_required");
        String key = "seen:" + scope(context);
        JSONObject seen = new JSONObject(prefs(context).getString(key, "{}"));
        if (seen.optLong(id) > now) return new JSONObject().put("accepted", true);
        Intent intent = new Intent(context, ConsoleActivity.class).setAction("com.ssine.codexnode.OPEN_CONVERSATION")
                .setData(Uri.parse("mira://conversation/" + id))
                .putExtra("threadId", thread).putExtra("serverUrl", NodeConfig.load(context).serverUrl)
                .addFlags(Intent.FLAG_ACTIVITY_CLEAR_TOP | Intent.FLAG_ACTIVITY_SINGLE_TOP);
        PendingIntent open = PendingIntent.getActivity(context, 0, intent, PendingIntent.FLAG_UPDATE_CURRENT | PendingIntent.FLAG_IMMUTABLE);
        channels(context);
        Notification notification = new Notification.Builder(context, CHANNEL).setSmallIcon(R.drawable.ic_launcher)
                .setContentTitle("对话已完成").setContentText(title).setContentIntent(open)
                .setAutoCancel(true).setOnlyAlertOnce(true).setVisibility(Notification.VISIBILITY_PRIVATE)
                .setCategory(Notification.CATEGORY_STATUS).build();
        context.getSystemService(NotificationManager.class).notify("completion:" + id, 1, notification);
        ArrayList<String> ids = new ArrayList<>(); seen.keys().forEachRemaining(ids::add);
        for (String item : ids) if (seen.optLong(item) <= now) seen.remove(item);
        ids.removeIf(item -> !seen.has(item)); ids.sort(Comparator.comparingLong(seen::optLong));
        while (ids.size() >= 512) seen.remove(ids.remove(0));
        seen.put(id, expiry);
        // A retry before this write replaces the same Android notification tag.
        if (!prefs(context).edit().putString(key, seen.toString()).commit()) throw new Exception("notification_state_failed");
        return new JSONObject().put("accepted", true);
    }
}
