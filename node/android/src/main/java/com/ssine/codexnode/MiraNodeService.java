package com.ssine.codexnode;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.app.Service;
import android.content.Context;
import android.content.Intent;
import android.content.pm.ApplicationInfo;
import android.content.pm.ServiceInfo;
import android.os.Build;
import android.os.IBinder;
import android.util.Base64;

import java.io.BufferedReader;
import java.io.File;
import java.io.FileReader;
import java.io.InputStreamReader;
import java.security.SecureRandom;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;

public final class MiraNodeService extends Service {
    private static final String ACTION_START = "com.ssine.codexnode.START";
    private static final String ACTION_PROBE_ROOT = "com.ssine.codexnode.PROBE_ROOT";
    private static final String ACTION_PROJECTION = "com.ssine.codexnode.PROJECTION";
    private static final String EXTRA_RESULT_CODE = "result_code";
    private static final String EXTRA_RESULT_DATA = "result_data";
    private static final String CHANNEL = "mira_node";
    private static final int NOTIFICATION_ID = 7101;
    private static volatile boolean running;

    private final ExecutorService workers = Executors.newCachedThreadPool();
    private final AtomicInteger generation = new AtomicInteger();
    private final Object nodeLifecycleLock = new Object();
    private final ScreenCaptureController capture = new ScreenCaptureController();
    private volatile Process nodeProcess;
    private volatile boolean desired;
    private AndroidBridgeServer bridge;
    private String bridgeToken;
    private int bridgePort;

    public static void start(Context context) {
        NodeConfig.setUserStopped(context, false);
        Intent intent = new Intent(context, MiraNodeService.class).setAction(ACTION_START);
        context.startForegroundService(intent);
    }

    static boolean isRunning() {
        return running;
    }

    public static void stop(Context context) {
        NodeConfig.setUserStopped(context, true);
        context.stopService(new Intent(context, MiraNodeService.class));
        publishStatus(context, "Stopped by user", "none");
    }

    public static void requestRootProbe(Context context) {
        Intent intent = new Intent(context, MiraNodeService.class).setAction(ACTION_PROBE_ROOT);
        context.startForegroundService(intent);
    }

    public static void grantProjection(Context context, int resultCode, Intent resultData) {
        Intent intent = new Intent(context, MiraNodeService.class).setAction(ACTION_PROJECTION)
                .putExtra(EXTRA_RESULT_CODE, resultCode).putExtra(EXTRA_RESULT_DATA, resultData);
        context.startForegroundService(intent);
    }

    static void publishStatus(Context context, String status, String mode) {
        android.content.SharedPreferences.Editor edit = context
                .getSharedPreferences(NodeConfig.PREFS, Context.MODE_PRIVATE).edit()
                .putString(NodeConfig.KEY_STATUS, status);
        if (mode != null) {
            edit.putString(NodeConfig.KEY_EFFECTIVE_MODE, mode);
        }
        edit.apply();
    }

    @Override public void onCreate() {
        super.onCreate();
        running = true;
        createNotificationChannel();
        startForegroundForNode("Starting");
        try {
            bridgeToken = randomToken();
            bridge = new AndroidBridgeServer(this, capture, bridgeToken);
            bridgePort = bridge.start();
        } catch (Exception error) {
            publishStatus(this, "Could not start Android API bridge: " + error.getMessage(), "none");
            stopSelf();
        }
    }

    @Override public int onStartCommand(Intent intent, int flags, int startId) {
        String action = intent == null ? ACTION_START : intent.getAction();
        if (ACTION_PROJECTION.equals(action)) {
            Intent data;
            if (Build.VERSION.SDK_INT >= 33) {
                data = intent.getParcelableExtra(EXTRA_RESULT_DATA, Intent.class);
            } else {
                data = intent.getParcelableExtra(EXTRA_RESULT_DATA);
            }
            int code = intent.getIntExtra(EXTRA_RESULT_CODE, 0);
            try {
                startForegroundForProjection();
                if (data == null) {
                    throw new Exception("screen capture result was empty");
                }
                capture.configure(this, code, data);
                publishStatus(this, "Screen capture ready; Mira Node continues running", null);
            } catch (Exception error) {
                publishStatus(this, "Screen capture failed: " + error.getMessage(), null);
            }
            return START_STICKY;
        }
        if (ACTION_PROBE_ROOT.equals(action)) {
            workers.execute(() -> {
                boolean root = probeRoot();
                publishStatus(this, root ? "Root authorization works" : "Root unavailable or denied", null);
            });
            return START_NOT_STICKY;
        }
        desired = true;
        restartNode();
        return START_STICKY;
    }

    private void restartNode() {
        int currentGeneration = generation.incrementAndGet();
        stopNodeProcess();
        workers.execute(() -> runNode(currentGeneration));
    }

    private void runNode(int currentGeneration) {
        NodeConfig config = NodeConfig.load(this);
        String validation = config.validate();
        if (validation != null) {
            publishStatus(this, validation, "none");
            return;
        }
        try {
            String effectiveMode = determineMode(config.requestedMode);
            File configFile = config.writeRuntimeFile(this, effectiveMode, bridgePort, bridgeToken);
            File executable = nativeNodePath();
            File openSSH;
            synchronized (nodeLifecycleLock) {
                if (!desired || currentGeneration != generation.get()) return;
                openSSH = OpenSSHRuntime.prepare(this, executable);
            }
            ProcessBuilder builder;
            if (effectiveMode.equals("root")) {
                File launcherPidFile = rootLauncherPidFile();
                launcherPidFile.delete();
                String command = "exec " + shellQuote(executable.getAbsolutePath())
                        + " --config " + shellQuote(configFile.getAbsolutePath());
                command = "export MIRA_NODE_OPENSSH_DIR=" + shellQuote(openSSH.getAbsolutePath())
                        + " MIRA_NODE_OPENSSH_ANDROID_ROOT=1; " + command;
                // Keep an app-UID shell as the Process owned by Java. Android
                // cannot signal the root process returned directly by `su`, but
                // it can always stop this shell; the native parent-exit guard
                // then terminates the privileged child.
                builder = new ProcessBuilder("/system/bin/sh", "-c",
                        "echo $$ > " + shellQuote(launcherPidFile.getAbsolutePath())
                                + "; su -c " + shellQuote(command));
            } else {
                rootLauncherPidFile().delete();
                builder = new ProcessBuilder(executable.getAbsolutePath(), "--config",
                        configFile.getAbsolutePath());
            }
            builder.environment().put("MIRA_NODE_OPENSSH_DIR", openSSH.getAbsolutePath());
            builder.environment().put("MIRA_NODE_OPENSSH_ANDROID_ROOT", "1");
            builder.redirectErrorStream(true);
            Process process;
            synchronized (nodeLifecycleLock) {
                // Package-replaced, accessibility and Activity recovery can all
                // request a start within the same few milliseconds. Only the
                // newest generation may cross the process-creation boundary.
                if (!desired || currentGeneration != generation.get()) {
                    return;
                }
                process = builder.start();
                nodeProcess = process;
            }
            publishStatus(this, "Running and connecting to " + config.serverUrl, effectiveMode);
            startForegroundForNode("Running in " + effectiveMode + " mode");
            try (BufferedReader lines = new BufferedReader(new InputStreamReader(process.getInputStream()))) {
                String line;
                while ((line = lines.readLine()) != null) {
                    if (line.contains("connected reverse capability channel")) {
                        publishStatus(this, "Connected as " + config.nodeKey, effectiveMode);
                    } else if (line.contains("registered Mira Node")) {
                        publishStatus(this, "Registered; opening reverse connection", effectiveMode);
                    } else if (line.contains("registration failed") || line.contains("reverse channel disconnected")
                            || line.contains("initial heartbeat failed") || line.contains("heartbeat failed")) {
                        String message = line;
                        try { message = new org.json.JSONObject(line).optString("error", line); }
                        catch (org.json.JSONException ignored) { }
                        publishStatus(this, abbreviate(message), effectiveMode);
                    }
                }
            }
            int exitCode = process.waitFor();
            synchronized (nodeLifecycleLock) {
                if (nodeProcess == process) {
                    nodeProcess = null;
                }
            }
            if (desired && currentGeneration == generation.get()) {
                publishStatus(this, "Mira Node exited with " + exitCode + "; reconnecting", effectiveMode);
                workers.execute(() -> {
                    try {
                        Thread.sleep(3000);
                    } catch (InterruptedException ignored) {
                        Thread.currentThread().interrupt();
                    }
                    if (desired && currentGeneration == generation.get()) {
                        runNode(currentGeneration);
                    }
                });
            }
        } catch (Exception error) {
            publishStatus(this, "Mira Node start failed: " + error.getMessage(), "none");
        }
    }

    private String determineMode(String requestedMode) throws Exception {
        if (requestedMode.equals("app")) {
            return "app";
        }
        boolean root = probeRoot();
        if (root) {
            return "root";
        }
        if (requestedMode.equals("root")) {
            throw new Exception("root was denied or no su provider is installed");
        }
        return "app";
    }

    private boolean probeRoot() {
        Process process = null;
        try {
            process = new ProcessBuilder("su", "-c", "id").redirectErrorStream(true).start();
            if (!process.waitFor(30, TimeUnit.SECONDS)) {
                process.destroyForcibly();
                return false;
            }
            try (BufferedReader output = new BufferedReader(new InputStreamReader(process.getInputStream()))) {
                String line;
                while ((line = output.readLine()) != null) {
                    if (line.contains("uid=0")) {
                        return process.exitValue() == 0;
                    }
                }
            }
        } catch (Exception ignored) {
        } finally {
            if (process != null && process.isAlive()) {
                process.destroyForcibly();
            }
        }
        return false;
    }

    private File nativeNodePath() throws Exception {
        ApplicationInfo info = getApplicationInfo();
        File executable = new File(info.nativeLibraryDir, "libmira_node.so");
        if (!executable.isFile() || !executable.canExecute()) {
            throw new Exception("packaged Mira Node binary is missing or not executable: " + executable);
        }
        return executable;
    }

    private void stopNodeProcess() {
        Process process;
        synchronized (nodeLifecycleLock) {
            process = nodeProcess;
            nodeProcess = null;
        }
        if (process != null && process.isAlive()) {
            Integer launcherPid = readRootLauncherPid();
            if (launcherPid != null) {
                // Process.destroy() is not reliable for this long-lived shell on
                // all Android builds. The launcher is still our app UID, so this
                // signal does not require root and its child exits via PPID guard.
                android.os.Process.killProcess(launcherPid);
            }
            process.destroy();
            try {
                if (!process.waitFor(2, TimeUnit.SECONDS)) {
                    process.destroyForcibly();
                }
            } catch (InterruptedException ignored) {
                Thread.currentThread().interrupt();
            }
        }
        rootLauncherPidFile().delete();
    }

    private File rootLauncherPidFile() {
        return new File(getNoBackupFilesDir(), "root-launcher.pid");
    }

    private Integer readRootLauncherPid() {
        File value = rootLauncherPidFile();
        if (!value.isFile()) {
            return null;
        }
        try (BufferedReader reader = new BufferedReader(new FileReader(value))) {
            int pid = Integer.parseInt(reader.readLine().trim());
            return pid > 1 ? pid : null;
        } catch (Exception ignored) {
            return null;
        }
    }

    private static String shellQuote(String value) {
        return "'" + value.replace("'", "'\\''") + "'";
    }

    private static String abbreviate(String value) {
        return value.length() <= 300 ? value : value.substring(0, 300) + "…";
    }

    private static String randomToken() {
        byte[] value = new byte[32];
        new SecureRandom().nextBytes(value);
        return Base64.encodeToString(value, Base64.NO_WRAP | Base64.NO_PADDING | Base64.URL_SAFE);
    }

    private void createNotificationChannel() {
        NotificationChannel channel = new NotificationChannel(CHANNEL, "Mira Node",
                NotificationManager.IMPORTANCE_LOW);
        channel.setDescription("Keeps this device connected to your private Mira control server");
        getSystemService(NotificationManager.class).createNotificationChannel(channel);
    }

    private Notification notification(String text) {
        PendingIntent open = PendingIntent.getActivity(this, 0, new Intent(this, MainActivity.class),
                PendingIntent.FLAG_UPDATE_CURRENT | PendingIntent.FLAG_IMMUTABLE);
        return new Notification.Builder(this, CHANNEL)
                .setContentTitle("Mira Node")
                .setContentText(text)
                .setSmallIcon(android.R.drawable.stat_notify_sync)
                .setContentIntent(open)
                .setOngoing(true)
                .build();
    }

    private void startForegroundForNode(String text) {
        Notification value = notification(text);
        if (Build.VERSION.SDK_INT >= 34) {
            startForeground(NOTIFICATION_ID, value, ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE);
        } else {
            startForeground(NOTIFICATION_ID, value);
        }
    }

    private void startForegroundForProjection() {
        Notification value = notification("Screen capture enabled");
        if (Build.VERSION.SDK_INT >= 34) {
            startForeground(NOTIFICATION_ID, value,
                    ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE
                            | ServiceInfo.FOREGROUND_SERVICE_TYPE_MEDIA_PROJECTION);
        } else if (Build.VERSION.SDK_INT >= 29) {
            startForeground(NOTIFICATION_ID, value,
                    ServiceInfo.FOREGROUND_SERVICE_TYPE_MEDIA_PROJECTION);
        } else {
            startForeground(NOTIFICATION_ID, value);
        }
    }

    @Override public void onDestroy() {
        running = false;
        desired = false;
        generation.incrementAndGet();
        stopNodeProcess();
        if (bridge != null) {
            bridge.close();
        }
        capture.close();
        workers.shutdownNow();
        publishStatus(this, "Stopped", "none");
        super.onDestroy();
    }

    @Override public IBinder onBind(Intent intent) {
        return null;
    }
}
