package com.ssine.codexnode;

import android.content.Context;
import android.content.Intent;
import android.content.IntentFilter;
import android.os.BatteryManager;
import android.os.StatFs;

import org.json.JSONArray;
import org.json.JSONObject;

/** App-accessible telemetry; shell dumpsys is not available to an app UID. */
final class AndroidStatusReader {
    static JSONObject read(Context context) throws Exception {
        JSONObject result = new JSONObject();
        JSONObject errors = new JSONObject();
        try {
            Intent battery = context.registerReceiver(null, new IntentFilter(Intent.ACTION_BATTERY_CHANGED));
            if (battery == null) throw new Exception("battery status unavailable");
            int level = battery.getIntExtra(BatteryManager.EXTRA_LEVEL, -1);
            int scale = battery.getIntExtra(BatteryManager.EXTRA_SCALE, -1);
            if (level < 0 || scale <= 0) throw new Exception("battery level unavailable");
            result.put("battery", "level: " + level + "\nscale: " + scale
                    + "\nplugged: " + battery.getIntExtra(BatteryManager.EXTRA_PLUGGED, 0)
                    + "\nstatus: " + battery.getIntExtra(BatteryManager.EXTRA_STATUS, -1));
        } catch (Exception error) {
            errors.put("battery", error.getMessage());
        }
        try {
            MiraAccessibilityService service = MiraAccessibilityService.connectedService();
            if (service == null) throw new Exception("accessibility_permission_required");
            result.put("focus", service.focusedApp());
        } catch (Exception error) {
            errors.put("focus", error.getMessage());
        }
        // / is Android's read-only system image, not the user's storage volume.
        String path = context.getNoBackupFilesDir().getAbsolutePath();
        JSONObject disk = new JSONObject().put("path", path);
        try {
            StatFs stats = new StatFs(path);
            long total = stats.getTotalBytes();
            long available = stats.getAvailableBytes();
            if (total <= 0 || available < 0 || available > total) {
                throw new Exception("storage capacity unavailable");
            }
            long used = total - available;
            disk.put("totalBytes", total).put("availableBytes", available)
                    .put("usedBytes", used).put("usagePercent", 100.0 * used / total);
        } catch (Exception error) {
            disk.put("error", error.getMessage());
        }
        return result.put("disk", new JSONArray().put(disk)).put("telemetryErrors", errors);
    }

    private AndroidStatusReader() { }
}
