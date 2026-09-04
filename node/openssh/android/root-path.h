/* Bundled Android root: retain upstream pre-auth chroot and UID/GID demotion.
 * The Node creates an empty private root-owned session directory, not /var/empty.
 */
#include <sys/stat.h>
#include <stdlib.h>
#include <stdio.h>
#include <unistd.h>
static const char *mira_privsep_path(void) {
    const char *path = getenv("MIRA_NODE_OPENSSH_PRIVSEP_DIR");
    struct stat st;
    if (!path || path[0] != '/' || lstat(path, &st) || !S_ISDIR(st.st_mode) ||
        st.st_uid != 0 || (st.st_mode & 022)) {
        fputs("invalid private root-owned OpenSSH chroot directory\n", stderr);
        _exit(70);
    }
    return path;
}
#undef _PATH_PRIVSEP_CHROOT_DIR
#define _PATH_PRIVSEP_CHROOT_DIR mira_privsep_path()
