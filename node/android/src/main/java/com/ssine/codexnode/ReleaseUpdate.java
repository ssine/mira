package com.ssine.codexnode;

import java.io.ByteArrayOutputStream;
import java.io.InputStream;
import java.net.HttpURLConnection;
import java.net.URL;
import java.nio.charset.StandardCharsets;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

/** Read-only update discovery. Android's package installer verifies the APK signing identity. */
final class ReleaseUpdate {
    private static final Pattern ANDROID_CHECKSUM = Pattern.compile(
            "(?m)^[a-fA-F0-9]{64}\\s+\\*?mira_((0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*))_android_arm64\\.apk[\\t ]*$");
    final String version;
    final String downloadUrl;
    final boolean newer;

    private ReleaseUpdate(String version, boolean newer) {
        this.version = version;
        this.newer = newer;
        this.downloadUrl = "https://github.com/ssine/mira/releases/download/v" + version
                + "/mira_" + version + "_android_arm64.apk";
    }

    private static String versionFromChecksumManifest(String manifest) throws Exception {
        Matcher match = ANDROID_CHECKSUM.matcher(manifest);
        if (!match.find()) throw new Exception("Latest release checksum has no Android arm64 APK");
        return match.group(1);
    }

    static ReleaseUpdate latest(String installed) throws Exception {
        HttpURLConnection connection = (HttpURLConnection) new URL(
                "https://github.com/ssine/mira/releases/latest/download/SHA256SUMS").openConnection();
        connection.setConnectTimeout(10000);
        connection.setReadTimeout(10000);
        connection.setInstanceFollowRedirects(true);
        connection.setRequestProperty("User-Agent", "Mira-Android/" + installed);
        try {
            if (connection.getResponseCode() != 200) throw new Exception("Release checksum lookup failed");
            ByteArrayOutputStream body = new ByteArrayOutputStream();
            try (InputStream stream = connection.getInputStream()) {
                byte[] buffer = new byte[8192];
                int count;
                while ((count = stream.read(buffer)) != -1) {
                    if (body.size() + count > 1024 * 1024) throw new Exception("Release response too large");
                    body.write(buffer, 0, count);
                }
            }
            String version = versionFromChecksumManifest(body.toString(StandardCharsets.UTF_8.name()));
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
