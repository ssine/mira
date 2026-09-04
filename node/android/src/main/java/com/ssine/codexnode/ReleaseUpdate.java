package com.ssine.codexnode;

import org.json.JSONArray;
import org.json.JSONObject;
import java.io.ByteArrayOutputStream;
import java.io.InputStream;
import java.net.HttpURLConnection;
import java.net.URL;
import java.nio.charset.StandardCharsets;

/** Read-only update discovery. Android's package installer verifies the APK signing identity. */
final class ReleaseUpdate {
    final String version;
    final String downloadUrl;
    final boolean newer;

    private ReleaseUpdate(String version, boolean newer) {
        this.version = version;
        this.newer = newer;
        this.downloadUrl = "https://github.com/ssine/mira/releases/download/v" + version
                + "/mira_" + version + "_android_arm64.apk";
    }

    static ReleaseUpdate latest(String installed) throws Exception {
        HttpURLConnection connection = (HttpURLConnection) new URL(
                "https://api.github.com/repos/ssine/mira/releases/latest").openConnection();
        connection.setConnectTimeout(10000);
        connection.setReadTimeout(10000);
        connection.setRequestProperty("Accept", "application/vnd.github+json");
        connection.setRequestProperty("User-Agent", "Mira-Android/" + installed);
        try {
            if (connection.getResponseCode() != 200) throw new Exception("Release lookup failed");
            ByteArrayOutputStream body = new ByteArrayOutputStream();
            try (InputStream stream = connection.getInputStream()) {
                byte[] buffer = new byte[8192];
                int count;
                while ((count = stream.read(buffer)) != -1) {
                    if (body.size() + count > 1024 * 1024) throw new Exception("Release response too large");
                    body.write(buffer, 0, count);
                }
            }
            JSONObject release = new JSONObject(body.toString(StandardCharsets.UTF_8.name()));
            String version = release.getString("tag_name").replaceFirst("^v", "");
            if (!version.matches("(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)")) {
                throw new Exception("Invalid release version");
            }
            String name = "mira_" + version + "_android_arm64.apk";
            JSONArray assets = release.getJSONArray("assets");
            boolean found = false;
            for (int index = 0; index < assets.length(); index++) {
                if (name.equals(assets.getJSONObject(index).getString("name"))) found = true;
            }
            if (!found) throw new Exception("APK not yet published");
            String[] current = installed.split("\\.");
            String[] target = version.split("\\.");
            boolean newer = false;
            for (int index = 0; index < 3; index++) {
                int comparison = Integer.compare(Integer.parseInt(target[index]), Integer.parseInt(current[index]));
                if (comparison != 0) { newer = comparison > 0; break; }
            }
            return new ReleaseUpdate(version, newer);
        } finally { connection.disconnect(); }
    }
}
