package com.ssine.codexnode;

import android.Manifest;
import android.annotation.SuppressLint;
import android.app.Activity;
import android.app.AlertDialog;
import android.content.Intent;
import android.content.SharedPreferences;
import android.content.pm.PackageManager;
import android.graphics.Typeface;
import android.media.projection.MediaProjectionManager;
import android.net.Uri;
import android.os.Build;
import android.os.Bundle;
import android.os.Environment;
import android.os.Handler;
import android.os.Looper;
import android.provider.Settings;
import android.text.InputType;
import android.view.View;
import android.widget.ArrayAdapter;
import android.widget.Button;
import android.widget.CheckBox;
import android.widget.EditText;
import android.widget.LinearLayout;
import android.widget.ScrollView;
import android.widget.Spinner;
import android.widget.TextView;
import android.widget.Toast;

@SuppressLint("SetTextI18n")
public final class MainActivity extends Activity {
    private static final int REQUEST_CAPTURE = 1001;
    private EditText serverInput;
    private EditText nodeInput;
    private Spinner modeInput;
    private CheckBox autoStartInput;
    private TextView statusView;
    private final Handler handler = new Handler(Looper.getMainLooper());
    private final Runnable statusUpdater = new Runnable() {
        @Override public void run() {
            updateStatus();
            handler.postDelayed(this, 1000);
        }
    };

    @Override public void onCreate(Bundle state) {
        super.onCreate(state);
        setContentView(buildContent());
        populate();
        requestNotificationPermission();
    }

    @Override protected void onResume() {
        super.onResume();
        handler.post(statusUpdater);
    }

    @Override protected void onPause() {
        handler.removeCallbacks(statusUpdater);
        super.onPause();
    }

    private View buildContent() {
        ScrollView scroll = new ScrollView(this);
        LinearLayout content = new LinearLayout(this);
        content.setOrientation(LinearLayout.VERTICAL);
        int padding = dp(20);
        content.setPadding(padding, padding, padding, padding);
        scroll.addView(content);
        // Android 15 enforces edge-to-edge; keep controls clear of system bars.
        scroll.setOnApplyWindowInsetsListener((view, insets) -> {
            view.setPadding(insets.getSystemWindowInsetLeft(), insets.getSystemWindowInsetTop(),
                    insets.getSystemWindowInsetRight(), insets.getSystemWindowInsetBottom());
            return insets;
        });

        TextView title = new TextView(this);
        title.setText("Mira Node");
        title.setTextSize(28);
        title.setTypeface(Typeface.DEFAULT, Typeface.BOLD);
        content.addView(title);

        TextView version = new TextView(this);
        version.setText("Version " + installedVersion());
        content.addView(version);

        TextView explanation = new TextView(this);
        explanation.setText("Enter your Mira Server URL, then Save and start. Approve this device on the Server website after checking its verification code. Auto mode uses authorized root when available, otherwise Android app permissions.");
        explanation.setPadding(0, dp(8), 0, dp(16));
        content.addView(explanation);

        serverInput = input(content, "Control Server URL", InputType.TYPE_CLASS_TEXT | InputType.TYPE_TEXT_VARIATION_URI);
        nodeInput = input(content, "Stable node key", InputType.TYPE_CLASS_TEXT);

        label(content, "Privilege mode");
        modeInput = new Spinner(this);
        String[] modes = new String[] {"Auto (root, then app)", "Root only", "App only"};
        modeInput.setAdapter(new ArrayAdapter<>(this, android.R.layout.simple_spinner_dropdown_item, modes));
        content.addView(modeInput);

        autoStartInput = new CheckBox(this);
        autoStartInput.setText("Start after reboot or app upgrade");
        content.addView(autoStartInput);

        statusView = new TextView(this);
        statusView.setPadding(0, dp(12), 0, dp(12));
        statusView.setTextIsSelectable(true);
        content.addView(statusView);

        button(content, "Save and start", view -> saveAndStart());
        button(content, "Stop", view -> MiraNodeService.stop(this));
        button(content, "Reset identity and enroll again", view -> resetIdentity());
        button(content, "Request / verify root", view -> MiraNodeService.requestRootProbe(this));
        button(content, "Enable non-root UI control", view ->
                startActivity(new Intent(Settings.ACTION_ACCESSIBILITY_SETTINGS)));
        button(content, "Grant non-root screen capture", view -> requestScreenCapture());
        button(content, "Grant shared-file access", view -> requestAllFilesAccess());
        button(content, "Check for updates", view -> checkForUpdates((Button) view));
        return scroll;
    }

    private String installedVersion() {
        try { return getPackageManager().getPackageInfo(getPackageName(), 0).versionName; }
        catch (PackageManager.NameNotFoundException impossible) { return "unknown"; }
    }

    private void checkForUpdates(Button button) {
        button.setEnabled(false);
        button.setText("Checking GitHub Releases…");
        new Thread(() -> {
            try {
                ReleaseUpdate release = ReleaseUpdate.latest(installedVersion());
                runOnUiThread(() -> {
                    if (isFinishing() || isDestroyed()) return;
                    button.setEnabled(true);
                    button.setText("Check for updates");
                    if (!release.newer) {
                        Toast.makeText(this, "Mira " + installedVersion() + " is up to date", Toast.LENGTH_LONG).show();
                        return;
                    }
                    new AlertDialog.Builder(this)
                            .setTitle("Mira " + release.version + " is available")
                            .setMessage("Download the signed APK in your browser and open it to confirm the update. Updates from an official Mira release preserve this device's identity and settings. Android may ask you to allow installation from your browser. Running sessions may disconnect during installation.")
                            .setNegativeButton("Later", null)
                            .setPositiveButton("Download APK", (dialog, which) -> {
                                try { startActivity(new Intent(Intent.ACTION_VIEW, Uri.parse(release.downloadUrl))); }
                                catch (RuntimeException error) { Toast.makeText(this, "No browser available", Toast.LENGTH_LONG).show(); }
                            }).show();
                });
            } catch (Exception error) {
                runOnUiThread(() -> {
                    if (isFinishing() || isDestroyed()) return;
                    button.setEnabled(true);
                    button.setText("Check for updates");
                    Toast.makeText(this, "Could not check GitHub Releases. Check your network and try again.", Toast.LENGTH_LONG).show();
                });
            }
        }, "mira-release-check").start();
    }

    private void resetIdentity() {
        MiraNodeService.stop(this);
        boolean removed = NodeConfig.resetIdentity(this);
        NodeConfig value = NodeConfig.load(this);
        NodeConfig.save(this, value.serverUrl, value.nodeKey, value.requestedMode, value.autoStart);
        Toast.makeText(this, removed ? "Node identity removed" : "No saved Node identity", Toast.LENGTH_LONG).show();
        updateStatus();
    }

    private void populate() {
        NodeConfig value = NodeConfig.load(this);
        serverInput.setText(value.serverUrl);
        nodeInput.setText(value.nodeKey);
        modeInput.setSelection(value.requestedMode.equals("root") ? 1 : value.requestedMode.equals("app") ? 2 : 0);
        autoStartInput.setChecked(value.autoStart);
        updateStatus();
    }

    private void saveAndStart() {
        String mode = modeInput.getSelectedItemPosition() == 1 ? "root"
                : modeInput.getSelectedItemPosition() == 2 ? "app" : "auto";
        NodeConfig value = NodeConfig.save(this, serverInput.getText().toString(),
                nodeInput.getText().toString(), mode,
                autoStartInput.isChecked());
        String error = value.validate();
        if (error != null) {
            Toast.makeText(this, error, Toast.LENGTH_LONG).show();
            return;
        }
        MiraNodeService.start(this);
    }

    private void requestScreenCapture() {
        MiraNodeService.start(this);
        MediaProjectionManager manager = (MediaProjectionManager) getSystemService(MEDIA_PROJECTION_SERVICE);
        startActivityForResult(manager.createScreenCaptureIntent(), REQUEST_CAPTURE);
    }

    @Override protected void onActivityResult(int requestCode, int resultCode, Intent data) {
        super.onActivityResult(requestCode, resultCode, data);
        if (requestCode == REQUEST_CAPTURE) {
            if (resultCode == RESULT_OK && data != null) {
                MiraNodeService.grantProjection(this, resultCode, data);
                Toast.makeText(this, "Screen capture granted for this service session", Toast.LENGTH_LONG).show();
            } else {
                Toast.makeText(this, "Screen capture was not granted", Toast.LENGTH_LONG).show();
            }
        }
    }

    private void requestAllFilesAccess() {
        if (Build.VERSION.SDK_INT < 30) {
            Toast.makeText(this, "Not required on this Android version", Toast.LENGTH_LONG).show();
            return;
        }
        Intent intent = new Intent(Settings.ACTION_MANAGE_APP_ALL_FILES_ACCESS_PERMISSION,
                Uri.parse("package:" + getPackageName()));
        startActivity(intent);
    }

    private void requestNotificationPermission() {
        if (Build.VERSION.SDK_INT >= 33
                && checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED) {
            requestPermissions(new String[] {Manifest.permission.POST_NOTIFICATIONS}, 1002);
        }
    }

    private void updateStatus() {
        SharedPreferences prefs = getSharedPreferences(NodeConfig.PREFS, MODE_PRIVATE);
        String status = prefs.getString(NodeConfig.KEY_STATUS, "Stopped");
        String mode = prefs.getString(NodeConfig.KEY_EFFECTIVE_MODE, "none");
        statusView.setText("Status: " + status + "\nEffective mode: " + mode
                + "\nAccessibility: " + (MiraAccessibilityService.isConnected() ? "ready" : "permission required")
                + "\nScreen capture: " + (ScreenCaptureController.isReady() ? "ready" : "permission required")
                + "\nShared files: " + (Build.VERSION.SDK_INT < 30 || Environment.isExternalStorageManager() ? "ready" : "app sandbox only"));
    }

    private EditText input(LinearLayout content, String title, int inputType) {
        label(content, title);
        EditText value = new EditText(this);
        value.setSingleLine(true);
        value.setInputType(inputType);
        content.addView(value);
        return value;
    }

    private void label(LinearLayout content, String value) {
        TextView label = new TextView(this);
        label.setText(value);
        label.setTypeface(Typeface.DEFAULT, Typeface.BOLD);
        label.setPadding(0, dp(10), 0, 0);
        content.addView(label);
    }

    private void button(LinearLayout content, String label, View.OnClickListener listener) {
        Button button = new Button(this);
        button.setText(label);
        button.setOnClickListener(listener);
        content.addView(button);
    }

    private int dp(int value) {
        return Math.round(value * getResources().getDisplayMetrics().density);
    }
}
