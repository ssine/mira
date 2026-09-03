package com.ssine.codexnode;

import android.content.Context;
import android.content.SharedPreferences;
import android.os.Build;
import android.os.Environment;

import org.json.JSONArray;
import org.json.JSONException;
import org.json.JSONObject;

import java.io.File;
import java.io.FileOutputStream;
import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.util.Locale;

final class AgentConfig {
    static final String PREFS = "codex_node";
    static final String KEY_SERVER = "server_url";
    static final String KEY_TOKEN = "server_token";
    static final String KEY_NODE = "node_key";
    static final String KEY_MODE = "privilege_mode";
    static final String KEY_AUTO_START = "auto_start";
    static final String KEY_USER_STOPPED = "user_stopped";
    static final String KEY_STATUS = "agent_status";
    static final String KEY_EFFECTIVE_MODE = "effective_mode";

    final String serverUrl;
    final String token;
    final String nodeKey;
    final String requestedMode;
    final boolean autoStart;

    private AgentConfig(String serverUrl, String token, String nodeKey, String requestedMode,
                        boolean autoStart) {
        this.serverUrl = serverUrl;
        this.token = token;
        this.nodeKey = nodeKey;
        this.requestedMode = requestedMode;
        this.autoStart = autoStart;
    }

    static AgentConfig load(Context context) {
        SharedPreferences prefs = context.getSharedPreferences(PREFS, Context.MODE_PRIVATE);
        return new AgentConfig(
                prefs.getString(KEY_SERVER, "http://127.0.0.1:8787"),
                prefs.getString(KEY_TOKEN, ""),
                prefs.getString(KEY_NODE, defaultNodeKey()),
                prefs.getString(KEY_MODE, "auto"),
                prefs.getBoolean(KEY_AUTO_START, false));
    }

    static AgentConfig save(Context context, String serverUrl, String token, String nodeKey,
                            String requestedMode, boolean autoStart) {
        String normalizedServer = serverUrl.trim().replaceAll("/+$", "");
        String normalizedNode = nodeKey.trim();
        context.getSharedPreferences(PREFS, Context.MODE_PRIVATE).edit()
                .putString(KEY_SERVER, normalizedServer)
                .putString(KEY_TOKEN, token)
                .putString(KEY_NODE, normalizedNode)
                .putString(KEY_MODE, requestedMode)
                .putBoolean(KEY_AUTO_START, autoStart)
                .apply();
        return new AgentConfig(normalizedServer, token, normalizedNode, requestedMode,
                autoStart);
    }

    static boolean isUserStopped(Context context) {
        return context.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
                .getBoolean(KEY_USER_STOPPED, false);
    }

    static void setUserStopped(Context context, boolean value) {
        context.getSharedPreferences(PREFS, Context.MODE_PRIVATE).edit()
                .putBoolean(KEY_USER_STOPPED, value)
                .apply();
    }

    String validate() {
        if (!(serverUrl.startsWith("http://") || serverUrl.startsWith("https://"))) {
            return "Server URL must start with http:// or https://";
        }
        if (token.isEmpty()) {
            return "Server bearer token is required";
        }
        if (nodeKey.isEmpty() || nodeKey.length() > 200) {
            return "Node key must contain between 1 and 200 characters";
        }
        if (!(requestedMode.equals("auto") || requestedMode.equals("root") || requestedMode.equals("app"))) {
            return "Unknown privilege mode";
        }
        return null;
    }

    File writeRuntimeFile(Context context, String effectiveMode, int bridgePort,
                          String bridgeToken) throws IOException, JSONException {
        JSONArray roots = new JSONArray();
        if (effectiveMode.equals("root")) {
            roots.put(Environment.getExternalStorageDirectory().getAbsolutePath());
            roots.put("/data/local/tmp");
        } else {
            roots.put(context.getFilesDir().getAbsolutePath());
            roots.put(context.getCacheDir().getAbsolutePath());
            File external = context.getExternalFilesDir(null);
            if (external != null) {
                roots.put(external.getAbsolutePath());
            }
            if (Build.VERSION.SDK_INT >= 30 && Environment.isExternalStorageManager()) {
                roots.put(Environment.getExternalStorageDirectory().getAbsolutePath());
            }
        }

        JSONObject value = new JSONObject();
        value.put("serverUrl", serverUrl);
        value.put("token", token);
        value.put("nodeKey", nodeKey);
        value.put("allowedRoots", roots);
        value.put("heartbeatSeconds", 3);
        value.put("privilegeMode", effectiveMode);
        value.put("exitWithParent", true);
        if (effectiveMode.equals("app")) {
            value.put("bridgeUrl", "http://127.0.0.1:" + bridgePort);
            value.put("bridgeToken", bridgeToken);
        }

        File destination = new File(context.getNoBackupFilesDir(), "agent-config.json");
        File temporary = new File(context.getNoBackupFilesDir(), "agent-config.json.tmp");
        byte[] encoded = value.toString().getBytes(StandardCharsets.UTF_8);
        try (FileOutputStream output = new FileOutputStream(temporary)) {
            output.write(encoded);
            output.getFD().sync();
        }
        if (!temporary.setReadable(false, false)
                || !temporary.setReadable(true, true)
                || !temporary.setWritable(false, false)
                || !temporary.setWritable(true, true)) {
            throw new IOException("Could not restrict runtime config permissions");
        }
        if (!temporary.renameTo(destination)) {
            throw new IOException("Could not install runtime config");
        }
        return destination;
    }

    private static String defaultNodeKey() {
        String label = (Build.MANUFACTURER + "-" + Build.MODEL).toLowerCase(Locale.ROOT);
        label = label.replaceAll("[^a-z0-9._-]+", "-").replaceAll("^-+|-+$", "");
        if (label.isEmpty()) {
            label = "android";
        }
        return "mira-android:" + label;
    }
}
