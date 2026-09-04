package com.ssine.codexnode;

import android.content.Context;
import android.system.Os;
import android.system.OsConstants;
import android.system.StructStat;

import java.io.File;

/** Role names are symlinks to the one APK-owned executable, never extra binaries. */
final class OpenSSHRuntime {
    static File prepare(Context context, File executable) throws Exception {
        File directory = new File(context.getNoBackupFilesDir(), "openssh-bin");
        if (!directory.mkdir() && !directory.isDirectory()) {
            throw new Exception("Could not create OpenSSH role directory");
        }
        StructStat stat = Os.lstat(directory.getAbsolutePath());
        if (!OsConstants.S_ISDIR(stat.st_mode) || stat.st_uid != android.os.Process.myUid()
                || (stat.st_mode & 0022) != 0) {
            throw new Exception("OpenSSH role directory is not app-owned and private");
        }
        Os.chmod(directory.getAbsolutePath(), 0700);
        // Android changes nativeLibraryDir after an APK update. Atomically replace
        // only our symlinks; never overwrite an unexpected regular file/directory.
        for (String role : new String[]{"mira", "ssh", "sshd", "sshd-session", "sshd-auth",
                "scp", "sftp", "sftp-server", "ssh-keygen"}) {
            File link = new File(directory, role);
            if (java.nio.file.Files.exists(link.toPath(), java.nio.file.LinkOption.NOFOLLOW_LINKS)
                    && !OsConstants.S_ISLNK(Os.lstat(link.getAbsolutePath()).st_mode)) {
                throw new Exception("Unexpected OpenSSH role file: " + role);
            }
            File temporary = new File(directory, role + ".new");
            if (java.nio.file.Files.exists(temporary.toPath(), java.nio.file.LinkOption.NOFOLLOW_LINKS)) {
                if (!OsConstants.S_ISLNK(Os.lstat(temporary.getAbsolutePath()).st_mode)) {
                    throw new Exception("Unexpected OpenSSH staging file: " + role);
                }
                java.nio.file.Files.delete(temporary.toPath());
            }
            Os.symlink(executable.getAbsolutePath(), temporary.getAbsolutePath());
            Os.rename(temporary.getAbsolutePath(), link.getAbsolutePath());
        }
        return directory;
    }

    private OpenSSHRuntime() { }
}
