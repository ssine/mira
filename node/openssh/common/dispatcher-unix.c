#define _GNU_SOURCE
#include <dirent.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#define ROLES(X) X(ssh, ssh) X(sshd, sshd) X(sshd_session, sshd-session) \
 X(sshd_auth, sshd-auth) X(scp, scp) X(sftp, sftp) \
 X(sftp_server, sftp-server) X(ssh_keygen, ssh-keygen)
#define DECLARE(id, name) extern int openssh_##id##_main(int, char **);
ROLES(DECLARE)
extern int mira_node_main(int, char **, int, char **);
extern void (*__start_mira_go_init[])(int, char **, char **);
extern void (*__stop_mira_go_init[])(int, char **, char **);
extern char **environ;

static int threads(void) {
    DIR *d = opendir("/proc/self/task");
    if (!d) return -1;
    int count = 0;
    struct dirent *entry;
    while ((entry = readdir(d))) if (entry->d_name[0] != '.') count++;
    closedir(d);
    return count;
}

int main(int argc, char **argv) {
    if (argc == 2 && !strcmp(argv[1], "--mira-openssh-build")) {
#if defined(__ANDROID__)
        puts("MIRA_LINKED_OPENSSH_ANDROID_ROOT_V1");
#else
        puts("MIRA_LINKED_OPENSSH_LINUX_STATIC_V1");
#endif
        return 0;
    }
    const char *role = strrchr(argv[0], '/');
    role = role ? role + 1 : argv[0];
    if (argc == 2 && !strcmp(argv[1], "--mira-dispatch-probe")) {
        printf("threads_before_go=%d\n", threads());
        return threads() == 1 ? 0 : 70;
    }
#define DISPATCH(id, name) if (!strcmp(role, #name)) { \
    if (threads() != 1) { fputs("unsafe OpenSSH entry: Go runtime already active\n", stderr); return 70; } \
    return openssh_##id##_main(argc, argv); }
    ROLES(DISPATCH)
    // Go's Linux c-archive startup reads auxv adjacent to the original argv.
    // Initialize it before constructing any synthetic CLI argument vector.
    for (size_t i = 0; i < (size_t)(__stop_mira_go_init - __start_mira_go_init); i++)
        __start_mira_go_init[i](argc, argv, environ);
    // Desktop staging may expose the embedded client as a "mira" alias.
    // Generated ProxyCommand invocations can already carry this prefix.
    if (!strcmp(role, "mira") && !(argc > 1 && !strcmp(argv[1], "cli"))) {
        char **forward = calloc((size_t)argc + 2, sizeof(char *));
        if (!forward) return 70;
        forward[0] = argv[0]; forward[1] = "cli";
        for (int i = 1; i < argc; i++) forward[i + 1] = argv[i];
        argv = forward; argc++;
    }
    int envc = 0;
    while (environ[envc]) envc++;
    return mira_node_main(argc, argv, envc, environ);
}
