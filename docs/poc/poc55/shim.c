/* LD_PRELOAD interposer for POC-55, built on POC-54's. It holds csql at chosen
 * points of its conversation with the server, so that a fault lands in a known
 * place, and writes one TSV line per call. It calls the kernel with syscall(),
 * which needs no dlsym and so loads on the image's older glibc.
 *
 * Environment:
 *   POC55_LOG   file to append to; unset means no log
 *   POC55_DIR   directory of the hold files
 *   POC55_HOLD  comma-separated hold points, taken in order, each kind:fn:nth
 *     connect:0:n   before the nth connect to the master's port or socket
 *     before:fn:n   before the nth request with function code fn goes out
 *     inflight:fn:n after that request went out, at the first poll for its reply
 *     after:fn:n    after its reply was read, before the next request goes out
 *
 * At hold point i the shim creates DIR/held.i, then waits for DIR/release.i,
 * for at most POC55_HOLD_MAX_S seconds (default 120).
 *
 * Lines: monotonic_ns, event, then fields:
 *   connect fd family address port rc
 *   send fd bytes type fn request_id
 *   poll fd events timeout_ms fn_outstanding enter_ns rc revents
 *   recv fd want got errno
 *   hold index kind fn nth
 *   released index waited_ms
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

#define MAX_POINTS 4
#define MAX_FN 400
enum { K_CONNECT, K_BEFORE, K_INFLIGHT, K_AFTER };

static struct { int kind, fn, nth; } points[MAX_POINTS];
static int log_fd = -2, n_points, cur, connects, armed, hold_max_s = 120;
static int seen[MAX_FN];
static volatile int last_fn = -1;
static const char *dir;

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
  p = getenv("POC55_LOG");
  log_fd = p ? (int) syscall(SYS_open, p, O_WRONLY | O_CREAT | O_APPEND, 0644) : -1;
  dir = getenv("POC55_DIR");
  if ((p = getenv("POC55_HOLD_MAX_S")))
    hold_max_s = atoi(p);
  if ((p = getenv("POC55_HOLD")))
    while (*p && n_points < MAX_POINTS)
      {
        char kind[16];
        int fn, nth, used = 0;
        if (sscanf(p, "%15[a-z]:%d:%d%n", kind, &fn, &nth, &used) != 3)
          break;
        points[n_points].kind = !strcmp(kind, "connect") ? K_CONNECT : !strcmp(kind, "before") ? K_BEFORE
                                : !strcmp(kind, "inflight") ? K_INFLIGHT : K_AFTER;
        points[n_points].fn = fn;
        points[n_points].nth = nth;
        n_points++;
        p += used;
        if (*p == ',')
          p++;
      }
}

static void out(const char *s, int n)
{
  int e = errno;
  if (log_fd >= 0)
    syscall(SYS_write, log_fd, s, n);
  errno = e;
}

static void hold(void)
{
  char path[256], line[120];
  long long t0 = now_ns(), limit = t0 + hold_max_s * 1000000000LL;
  struct timespec ms = { 0, 1000000L };
  int e = errno, i = cur, fd, len;
  len = snprintf(line, sizeof line, "%lld\thold\t%d\t%d\t%d\t%d\n", t0, i, points[i].kind, points[i].fn, points[i].nth);
  out(line, len);
  cur++;
  armed = 0;
  if (dir)
    {
      snprintf(path, sizeof path, "%s/held.%d", dir, i);
      fd = (int) syscall(SYS_open, path, O_WRONLY | O_CREAT, 0644);
      if (fd >= 0)
        syscall(SYS_close, fd);
      snprintf(path, sizeof path, "%s/release.%d", dir, i);
      while (syscall(SYS_access, path, F_OK) != 0 && now_ns() < limit)
        syscall(SYS_nanosleep, &ms, NULL);
    }
  len = snprintf(line, sizeof line, "%lld\treleased\t%d\t%lld\n", now_ns(), i, (now_ns() - t0) / 1000000);
  out(line, len);
  errno = e;
}

/* NET_HEADER in connection_defs.h: type, version, host_id, transaction_id,
 * request_id, db_error, then function_code at byte 24, in network order. Type 1
 * is a command, and a request's data packets are of another type. */
static int header_fn(const void *p, size_t n, int *rid)
{
  int type = -1;
  unsigned short f;
  if (n < 32)
    return -1;
  memcpy(&type, p, 4);
  if (ntohl(type) != 1)
    return -1;
  memcpy(&f, (const char *) p + 24, 2);
  memcpy(rid, (const char *) p + 16, 4);
  *rid = ntohl(*rid);
  return ntohs(f);
}

static void before_request(int fn)
{
  if (cur < n_points && points[cur].kind == K_AFTER && armed)
    hold();
  if (fn >= 0 && fn < MAX_FN)
    seen[fn]++;
  if (cur < n_points && points[cur].kind == K_BEFORE && points[cur].fn == fn && seen[fn] == points[cur].nth)
    hold();
  last_fn = fn;
  if (cur < n_points && points[cur].fn == fn && seen[fn] == points[cur].nth
      && (points[cur].kind == K_INFLIGHT || points[cur].kind == K_AFTER))
    armed = 1;
}

static void note_send(int fd, int fn, int rid, long total)
{
  char line[160];
  int len = snprintf(line, sizeof line, "%lld\tsend\t%d\t%ld\t%d\t%d\n", now_ns(), fd, total, fn, rid);
  out(line, len);
}

int connect(int fd, const struct sockaddr *a, socklen_t l)
{
  char line[200], addr[120] = "-";
  int rc, len, port = 0, fam = a ? a->sa_family : -1, master = 0;
  setup();
  if (fam == AF_INET)
    {
      const struct sockaddr_in *in = (const void *) a;
      inet_ntop(AF_INET, &in->sin_addr, addr, sizeof addr);
      port = ntohs(in->sin_port);
      master = port == 1523;
    }
  else if (fam == AF_UNIX)
    {
      snprintf(addr, sizeof addr, "%s", ((const struct sockaddr_un *) a)->sun_path);
      master = strstr(addr, "CUBRID") != NULL;
    }
  if (master)
    {
      connects++;
      if (cur < n_points && points[cur].kind == K_CONNECT && points[cur].nth == connects)
        hold();
    }
  rc = (int) syscall(SYS_connect, fd, a, l);
  len = snprintf(line, sizeof line, "%lld\tconnect\t%d\t%d\t%s\t%d\t%d\n", now_ns(), fd, fam, addr, port, rc);
  out(line, len);
  return rc;
}

ssize_t send(int fd, const void *b, size_t n, int flags)
{
  ssize_t rc;
  int rid = 0, fn;
  setup();
  fn = header_fn(b, n, &rid);
  if (fn >= 0)
    before_request(fn);
  rc = syscall(SYS_sendto, fd, b, n, flags, NULL, 0);
  note_send(fd, fn, rid, rc);
  return rc;
}

ssize_t writev(int fd, const struct iovec *v, int c)
{
  ssize_t rc;
  int i, rid = 0, fn = -1;
  setup();
  /* css_send_io_vector sends a 4-byte length, then the 32-byte NET_HEADER. */
  for (i = 0; i < c; i++)
    if (v[i].iov_len == 32 && (fn = header_fn(v[i].iov_base, v[i].iov_len, &rid)) >= 0)
      break;
  if (fn >= 0)
    before_request(fn);
  rc = syscall(SYS_writev, fd, v, c);
  note_send(fd, fn, rid, rc);
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
  errno = e;
  return rc;
}

int poll(struct pollfd *p, nfds_t c, int timeout)
{
  char line[200];
  long long t0, t1;
  int rc, len, e;
  setup();
  if (cur < n_points && points[cur].kind == K_INFLIGHT && armed && c == 1 && (p[0].events & POLLIN))
    hold();
  t0 = now_ns();
  rc = (int) syscall(SYS_poll, p, c, timeout);
  e = rc < 0 ? errno : 0;
  t1 = now_ns();
  len = snprintf(line, sizeof line, "%lld\tpoll\t%d\t%d\t%d\t%d\t%lld\t%d\t0x%x\n", t1, c ? p[0].fd : -1,
                 c ? p[0].events : 0, timeout, last_fn, t0, rc, c ? p[0].revents : 0);
  out(line, len);
  errno = e;
  return rc;
}
