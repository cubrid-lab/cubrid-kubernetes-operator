/* LD_PRELOAD interposer for POC-54. It wraps the socket calls csql makes, delays
 * poll() as an arm asks and writes one TSV line per call, so a run's file shows
 * what each poll returned and which request was outstanding. It calls the kernel
 * with syscall(), which needs no dlsym and so loads on the image's older glibc.
 *
 * Environment:
 *   POC54_LOG      file to append to; unset means no log
 *   POC54_DELAY_US delay before a poll; default 0
 *   POC54_WHEN     "all": every poll; "last": only polls after the request
 *                  POC54_TARGET_FN was sent; "once": only the first read poll
 *                  after that request; default "all"
 *   POC54_TARGET_FN the function code of NET_SERVER_BO_UNREGISTER_CLIENT, 4
 *   POC54_STRIP_HUP 1: when poll reports POLLIN with POLLHUP, hide POLLHUP, so
 *                  css_readn goes on to recv as it would on an open socket
 *
 * Lines: monotonic_ns, event, then fields:
 *   connect fd family address port rc
 *   send fd bytes type fn request_id
 *   poll fd events timeout_ms delayed_us fn_outstanding enter_ns rc revents stripped
 *   recv fd want got errno
 */
#define _GNU_SOURCE
#include <arpa/inet.h>
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/syscall.h>
#include <sys/uio.h>
#include <sys/un.h>
#include <time.h>
#include <unistd.h>

static int log_fd = -2, delay_us, only_last, only_once, once_done, target_fn = 4, strip_hup;
static volatile int last_fn = -1, target_seen;

static long long now_ns(void)
{
  struct timespec t;
  syscall(SYS_clock_gettime, CLOCK_MONOTONIC, &t);
  return t.tv_sec * 1000000000LL + t.tv_nsec;
}

static void setup(void)
{
  const char *p;
  if (log_fd != -2)
    return;
  p = getenv("POC54_LOG");
  log_fd = p ? (int) syscall(SYS_open, p, O_WRONLY | O_CREAT | O_APPEND, 0644) : -1;
  if ((p = getenv("POC54_DELAY_US")))
    delay_us = atoi(p);
  if ((p = getenv("POC54_WHEN")))
    {
      only_last = !strcmp(p, "last");
      only_once = !strcmp(p, "once");
    }
  if ((p = getenv("POC54_TARGET_FN")))
    target_fn = atoi(p);
  if ((p = getenv("POC54_STRIP_HUP")))
    strip_hup = atoi(p);
}

static void out(const char *s, int n)
{
  int e = errno;
  if (log_fd >= 0)
    syscall(SYS_write, log_fd, s, n);
  errno = e;
}

static void note_request(int fd, const void *p, size_t n, long total)
{
  /* NET_HEADER in connection_defs.h: type, version, host_id, transaction_id,
   * request_id, db_error, then function_code at byte 24, in network order. */
  char line[160];
  int type = -1, fn = -1, rid = -1, len;
  if (n >= 32)
    {
      memcpy(&type, p, 4);
      type = ntohl(type);
      if (type == 1)
        {
          unsigned short f;
          memcpy(&f, (const char *) p + 24, 2);
          fn = ntohs(f);
          memcpy(&rid, (const char *) p + 16, 4);
          rid = ntohl(rid);
          last_fn = fn;
          if (fn == target_fn)
            target_seen = 1;
        }
    }
  len = snprintf(line, sizeof line, "%lld\tsend\t%d\t%ld\t%d\t%d\t%d\n", now_ns(), fd, total, type, fn, rid);
  out(line, len);
}

int connect(int fd, const struct sockaddr *a, socklen_t l)
{
  char line[200], addr[120] = "-";
  int rc, len, port = 0, fam = a ? a->sa_family : -1;
  setup();
  if (fam == AF_INET)
    {
      const struct sockaddr_in *in = (const void *) a;
      inet_ntop(AF_INET, &in->sin_addr, addr, sizeof addr);
      port = ntohs(in->sin_port);
    }
  else if (fam == AF_UNIX)
    snprintf(addr, sizeof addr, "%s", ((const struct sockaddr_un *) a)->sun_path);
  rc = (int) syscall(SYS_connect, fd, a, l);
  len = snprintf(line, sizeof line, "%lld\tconnect\t%d\t%d\t%s\t%d\t%d\n", now_ns(), fd, fam, addr, port, rc);
  out(line, len);
  return rc;
}

ssize_t send(int fd, const void *b, size_t n, int flags)
{
  ssize_t rc;
  setup();
  rc = syscall(SYS_sendto, fd, b, n, flags, NULL, 0);
  note_request(fd, b, n, rc);
  return rc;
}

ssize_t writev(int fd, const struct iovec *v, int c)
{
  ssize_t rc;
  int i;
  setup();
  rc = syscall(SYS_writev, fd, v, c);
  /* css_send_io_vector sends a 4-byte length, then the 32-byte NET_HEADER. */
  for (i = 0; i < c; i++)
    if (v[i].iov_len == 32)
      {
        note_request(fd, v[i].iov_base, v[i].iov_len, rc);
        return rc;
      }
  note_request(fd, "", 0, rc);
  return rc;
}

ssize_t recv(int fd, void *b, size_t n, int flags)
{
  char line[120];
  ssize_t rc;
  int e, len;
  setup();
  rc = syscall(SYS_recvfrom, fd, b, n, flags, NULL, NULL);
  e = rc < 0 ? errno : 0;
  len = snprintf(line, sizeof line, "%lld\trecv\t%d\t%zu\t%zd\t%d\n", now_ns(), fd, n, rc, e);
  out(line, len);
  return rc;
}

int poll(struct pollfd *p, nfds_t c, int timeout)
{
  char line[200];
  long long t0, t1;
  int rc, len, d, stripped = 0, e;
  setup();
  if (only_once)
    {
      d = (target_seen && !once_done && c == 1 && (p[0].events & POLLIN)) ? delay_us : 0;
      if (d)
        once_done = 1;
    }
  else
    d = (!only_last || target_seen) ? delay_us : 0;
  t0 = now_ns();
  if (d > 0)
    {
      struct timespec ts = { d / 1000000, (d % 1000000) * 1000L };
      syscall(SYS_nanosleep, &ts, NULL);
    }
  rc = (int) syscall(SYS_poll, p, c, timeout);
  e = rc < 0 ? errno : 0;
  t1 = now_ns();
  len = snprintf(line, sizeof line, "%lld\tpoll\t%d\t%d\t%d\t%d\t%d\t%lld\t%d\t0x%x", t1, c ? p[0].fd : -1,
                 c ? p[0].events : 0, timeout, d, last_fn, t0, rc, c ? p[0].revents : 0);
  if (strip_hup && rc > 0 && c == 1 && (p[0].revents & POLLIN) && (p[0].revents & POLLHUP))
    {
      p[0].revents &= ~POLLHUP;
      stripped = 1;
    }
  len += snprintf(line + len, sizeof line - len, "\t%d\n", stripped);
  out(line, len);
  errno = e;
  return rc;
}
