package com.ssine.codexnode;

import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;

public final class BootReceiver extends BroadcastReceiver {
    @Override public void onReceive(Context context, Intent intent) {
        String action = intent.getAction();
        boolean ownPackageReplaced = Intent.ACTION_PACKAGE_REPLACED.equals(action)
                && intent.getData() != null
                && context.getPackageName().equals(intent.getData().getSchemeSpecificPart());
        if ((Intent.ACTION_BOOT_COMPLETED.equals(action)
                || Intent.ACTION_MY_PACKAGE_REPLACED.equals(action)
                || ownPackageReplaced)
                && NodeConfig.load(context).autoStart
                && !NodeConfig.isUserStopped(context)) {
            MiraNodeService.start(context);
        }
    }
}
