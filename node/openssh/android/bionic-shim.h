/* Bionic guards the API-26 futimes declaration behind BSD feature visibility. */
#ifndef _BSD_SOURCE
#define _BSD_SOURCE 1
#endif
#include "config.h"
#include <string.h>
#include <strings.h>
#ifndef _PATH_MAILDIR
#define _PATH_MAILDIR "/nonexistent/mira-mail"
#endif
/* Bionic's function-like macro cannot be used as a function pointer by OpenSSH. */
#ifdef bzero
#undef bzero
#define bzero __bionic_bzero
#endif
