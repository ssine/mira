#include <windows.h>
#include <tlhelp32.h>
#include <stdio.h>
#include <stdlib.h>
#include <wchar.h>
#define ROLES(X) X(ssh, L"ssh.exe") X(sshd,L"sshd.exe") X(sshd_session,L"sshd-session.exe") \
 X(sshd_auth,L"sshd-auth.exe") X(scp,L"scp.exe") X(sftp,L"sftp.exe") \
 X(sftp_server,L"sftp-server.exe") X(ssh_keygen,L"ssh-keygen.exe") X(ssh_shellhost,L"ssh-shellhost.exe")
#define EXTRA_ROLES(X) X(ssh_agent,L"ssh-agent.exe") X(ssh_add,L"ssh-add.exe") X(ssh_keyscan,L"ssh-keyscan.exe") X(ssh_sk_helper,L"ssh-sk-helper.exe") X(ssh_pkcs11_helper,L"ssh-pkcs11-helper.exe")
#define DECLARE(id,name) extern int openssh_##id##_wmain(int,wchar_t**);
ROLES(DECLARE)
EXTRA_ROLES(DECLARE)
extern int mira_node_main(int,char**,int,char**);
extern void (*const mira_go_constructor)(void);
static int threads(void){
    HANDLE snapshot=CreateToolhelp32Snapshot(TH32CS_SNAPTHREAD,0);
    if(snapshot==INVALID_HANDLE_VALUE)return -1;
    THREADENTRY32 entry={0};entry.dwSize=sizeof(entry);int count=0;
    if(Thread32First(snapshot,&entry))do{if(entry.th32OwnerProcessID==GetCurrentProcessId())count++;}while(Thread32Next(snapshot,&entry));
    CloseHandle(snapshot);return count;
}
int wmain(int argc,wchar_t **argv){
    if(argc==2&&!wcscmp(argv[1],L"--mira-openssh-build")){puts("MIRA_LINKED_OPENSSH_WINDOWS_FULL_V1");return 0;}
    const wchar_t *role=argv[0];
    // Upstream's re-exec paths use '/', while Go and the Windows shell use '\\'.
    for(const wchar_t *p=argv[0];*p;p++)if(*p==L'\\'||*p==L'/')role=p+1;
    if(argc==2&&!wcscmp(argv[1],L"--mira-dispatch-probe")){printf("threads_before_go=%d\n",threads());return 0;}
#define DISPATCH(id,name) if(!_wcsicmp(role,name))return openssh_##id##_wmain(argc,argv);
    ROLES(DISPATCH)
    EXTRA_ROLES(DISPATCH)
    // Mira's generated ProxyCommand may already include the embedded "cli"
    // prefix. Do not add it a second time when this executable re-enters.
    int cli=!_wcsicmp(role,L"mira.exe") && !(argc>1&&!wcscmp(argv[1],L"cli"));
    char **utf8=calloc(argc+cli+1,sizeof(char*));if(!utf8)return 70;
    for(int i=0;i<argc;i++){int j=i+(cli&&i>0);int size=WideCharToMultiByte(CP_UTF8,WC_ERR_INVALID_CHARS,argv[i],-1,NULL,0,NULL,NULL);if(!size)return 70;utf8[j]=malloc(size);if(!utf8[j]||!WideCharToMultiByte(CP_UTF8,WC_ERR_INVALID_CHARS,argv[i],-1,utf8[j],size,NULL,NULL))return 70;}
    if(cli)utf8[1]="cli";
    mira_go_constructor();
    return mira_node_main(argc+cli,utf8,0,NULL);
}
