#include <stdbool.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <libkrun.h>

#define CHECK(name, expr) do { int r_ = (expr); fprintf(stderr, "launcher: %s -> %d\n", name, r_); if (r_ < 0) return 1; } while (0)

/* usage: launcher ROOT SHARED SOCK TAP|- MAC */
int main(int argc, char **argv) {
  if (argc != 6) { fprintf(stderr, "usage: launcher ROOT SHARED SOCK TAP MAC\n"); return 2; }
  unsigned int m[6];
  uint8_t mac[6];
  if (sscanf(argv[5], "%x:%x:%x:%x:%x:%x", &m[0], &m[1], &m[2], &m[3], &m[4], &m[5]) != 6) return 2;
  for (int i = 0; i < 6; i++) mac[i] = (uint8_t)m[i];

  krun_set_log_level(getenv("P2_LOG") ? atoi(getenv("P2_LOG")) : 2);
  int ctx = krun_create_ctx();
  CHECK("krun_create_ctx", ctx);
  CHECK("krun_set_vm_config", krun_set_vm_config(ctx, 2, 512));
  CHECK("krun_set_root", krun_set_root(ctx, argv[1]));
  CHECK("krun_add_virtiofs", krun_add_virtiofs(ctx, "shared", argv[2]));
  const char *envp[] = { NULL };
  CHECK("krun_set_env", krun_set_env(ctx, envp));
  CHECK("krun_disable_implicit_vsock", krun_disable_implicit_vsock(ctx));
  CHECK("krun_add_vsock", krun_add_vsock(ctx, 0));
  CHECK("krun_add_vsock_port2", krun_add_vsock_port2(ctx, 1024, argv[3], true));
  if (strcmp(argv[4], "-") != 0) {
    CHECK("krun_add_net_tap", krun_add_net_tap(ctx, argv[4], mac, COMPAT_NET_FEATURES, 0));
  }
  CHECK("krun_start_enter", krun_start_enter(ctx));
  return 0;
}
