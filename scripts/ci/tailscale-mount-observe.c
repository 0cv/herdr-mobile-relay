/* Finite scratch-only observer. Never prints a path, device number or mount row.
 * Linux: mountinfo is not a promise that st_dev alone identifies a mount.
 * Darwin: getfsstat/fstatfs fsid and mountpoint spelling are observational;
 * APFS firmlinks and other aliases defeat naive pathname-prefix mapping.
 * No alias observation implies an atomic or universal admission algorithm.
 */
#define _GNU_SOURCE
#include <sys/types.h>
#include <sys/stat.h>
#include <sys/statvfs.h>
#ifdef __APPLE__
#include <sys/mount.h>
#else
#include <sys/sysmacros.h>
#endif
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <limits.h>

static int same_inode(const struct stat *a, const struct stat *b) {
    return a->st_dev == b->st_dev && a->st_ino == b->st_ino;
}

#ifndef __APPLE__
/* Only the four kernel mountinfo path escapes are accepted. An unknown escape
 * or a truncated line makes the entire inventory unavailable, not negative. */
static int decode(char *s) {
    char *in = s, *out = s;
    while (*in) {
        if (*in != '\\') { *out++ = *in++; continue; }
        if (in[1] != '0' || !in[2] || !in[3] || !in[4]) return 0;
        if (!strncmp(in + 1, "040", 3)) *out++ = ' ';
        else if (!strncmp(in + 1, "011", 3)) *out++ = '\t';
        else if (!strncmp(in + 1, "012", 3)) *out++ = '\n';
        else if (!strncmp(in + 1, "134", 3)) *out++ = '\\';
        else return 0;
        in += 4;
    }
    *out = 0;
    return 1;
}

static int inventory(const char *root, const char *alias, dev_t device,
                     const char *cross_path, dev_t cross_device,
                     int *count, int *alias_match) {
    FILE *f = fopen("/proc/self/mountinfo", "r");
    if (!f) return 0;
    char line[8192], mountpoint[4096], id[64], parent[64], majmin[64], source[4096];
    int ok = 1, root_device_seen = 0, cross_device_seen = 0, matched_alias = 0;
    size_t total = 0, longest = 0, cross_longest = 0;
    while (fgets(line, sizeof line, f)) {
        size_t n = strlen(line);
        total += n;
        if (++*count > 4096 || total > 1048576 || !n || line[n - 1] != '\n') { ok = 0; break; }
        /* First five mandatory fields, no arbitrary text emitted. Require the
         * separator and validate major:minor for any candidate scratch entry. */
        if (!strstr(line, " - ") || sscanf(line, "%63s %63s %63s %4095s %4095s", id, parent, majmin, source, mountpoint) != 5 || !decode(mountpoint)) {
            ok = 0; break;
        }
        size_t m = strlen(mountpoint);
        int covers = (m == 1 && mountpoint[0] == '/') ||
            (m <= strlen(root) && !strncmp(root, mountpoint, m) && (root[m] == '/' || root[m] == 0));
        int covers_cross = cross_path && ((m == 1 && mountpoint[0] == '/') ||
            (m <= strlen(cross_path) && !strncmp(cross_path, mountpoint, m) &&
             (cross_path[m] == '/' || cross_path[m] == 0)));
        if (!covers && !covers_cross && strcmp(mountpoint, alias)) continue;
        unsigned major_id, minor_id;
        char extra;
        if (sscanf(majmin, "%u:%u%c", &major_id, &minor_id, &extra) != 2) { ok = 0; break; }
        if (covers && m >= longest) {
            longest = m;
            root_device_seen = major(device) == major_id && minor(device) == minor_id;
        }
        if (covers_cross && m >= cross_longest) {
            cross_longest = m;
            cross_device_seen = major(cross_device) == major_id && minor(cross_device) == minor_id;
        }
        if (strcmp(mountpoint, alias) == 0 && major(device) == major_id && minor(device) == minor_id) matched_alias++;
    }
    if (ferror(f)) ok = 0;
    fclose(f);
    /* A covering mountinfo device is required, not just a successful read.
     * Duplicate/ambiguous exact alias entries are NOT a passing negative. */
    *alias_match = matched_alias == 1;
    return ok && *count > 0 && longest && root_device_seen &&
           (!cross_path || (cross_longest && cross_device_seen));
}
#else
static int inventory(const fsid_t *fsid, int *count) {
    /* MNT_NOWAIT avoids an update request. Cap allocation AND returned count;
     * lack of an exact stable inventory is inconclusive. Never print names. */
    int n = getfsstat(NULL, 0, MNT_NOWAIT);
    if (n <= 0 || n > 4096) return 0;
    struct statfs *entries = calloc((size_t)n + 1, sizeof *entries);
    if (!entries) return 0;
    int actual = getfsstat(entries, (int)((n + 1) * sizeof *entries), MNT_NOWAIT);
    int match = 0;
    if (actual > 0 && actual <= n) {
        for (int i = 0; i < actual; ++i)
            if (memcmp(&entries[i].f_fsid, fsid, sizeof *fsid) == 0) ++match;
    }
    free(entries);
    if (actual <= 0 || actual > n) return 0;
    *count = actual;
    return match > 0;  /* fsid match, NOT path-spelling or mount-boundary proof */
}
#endif

int main(int argc, char **argv) {
    if ((argc != 2 && argc != 3) || geteuid() == 0 || !getenv("GITHUB_ACTIONS") ||
        strcmp(getenv("GITHUB_ACTIONS"), "true") || !getenv("RUNNER_ENVIRONMENT") ||
        strcmp(getenv("RUNNER_ENVIRONMENT"), "github-hosted")) return 2;
    const char *root = argv[1];
    size_t n = strlen(root);
    if (n < 10 || n > 480 || root[0] != '/' || strstr(root, "/../") || strstr(root, "/./")) return 2;
    char ordinary[512], link[512], alias[512];
    if (snprintf(ordinary, sizeof ordinary, "%s/ordinary", root) >= (int)sizeof ordinary ||
        snprintf(link, sizeof link, "%s/link", root) >= (int)sizeof link ||
        snprintf(alias, sizeof alias, "%s/alias", root) >= (int)sizeof alias) return 2;
    struct stat r, a, p, l, lp;
    int rootfd = open(root, O_RDONLY | O_DIRECTORY | O_NOFOLLOW);
    if (rootfd < 0) return 2;
    int dirfd = openat(rootfd, "ordinary", O_RDONLY | O_DIRECTORY | O_NOFOLLOW);
    int linkfd = openat(rootfd, "link", O_RDONLY | O_DIRECTORY | O_NOFOLLOW);
    int aliasfd = openat(rootfd, "alias", O_RDONLY | O_DIRECTORY | O_NOFOLLOW);
    if (fstat(rootfd, &r) || dirfd < 0 || aliasfd < 0 || fstat(dirfd, &a) ||
        stat(ordinary, &p) || lstat(link, &l)) return 2;
    int ordinary_ok = S_ISDIR(a.st_mode) && r.st_uid == geteuid() &&
                      a.st_uid == geteuid() && same_inode(&a, &p);
    int symlink_refused = linkfd < 0 && S_ISLNK(l.st_mode) && l.st_uid == geteuid() &&
                          stat(link, &lp) == 0 && same_inode(&a, &lp);
    int fd_path = same_inode(&a, &p);
    int cross = 2; /* no stock writable second filesystem: unsupported */
#ifndef __APPLE__
    dev_t cross_device = 0;
    if (argc == 3) {
        const char *q = argv[2];
        if (strncmp(q, "/dev/shm/herdr-diagnostic-", 26) || !q[26] ||
            strlen(q) > 240 || strchr(q + 26, '/')) return 2;
        int crossfd = open(q, O_RDONLY | O_DIRECTORY | O_NOFOLLOW);
        struct stat child_fd, child_path;
        if (crossfd < 0 || fstat(crossfd, &child_fd) || stat(q, &child_path)) return 2;
        cross_device = child_fd.st_dev;
        cross = S_ISDIR(child_fd.st_mode) && child_fd.st_uid == geteuid() &&
                same_inode(&child_fd, &child_path) && child_fd.st_dev != a.st_dev;
        close(crossfd);
    }
#else
    if (argc != 2) return 2;
#endif
    int entries = 0, available, alias_ok = 0;
#ifdef __APPLE__
    struct statfs fd_fs, path_fs;
    available = fstatfs(dirfd, &fd_fs) == 0 && statfs(ordinary, &path_fs) == 0 &&
                memcmp(&fd_fs.f_fsid, &path_fs.f_fsid, sizeof fd_fs.f_fsid) == 0 &&
                inventory(&fd_fs.f_fsid, &entries);
    /* f_mntonname may not be the spelling of root/ordinary (APFS firmlink).
     * There is no stock nullfs alias assumed here; alias stays unsupported. */
#else
    struct stat b, bp;
    available = inventory(root, alias, r.st_dev, argc == 3 ? argv[2] : NULL,
                          cross_device, &entries, &alias_ok);
    if (fstat(aliasfd, &b) || stat(alias, &bp)) return 2;
    alias_ok = alias_ok && same_inode(&a, &b) && same_inode(&b, &bp);
#endif
    if (entries > 4096) return 2;
    printf("ordinary=%d symlink_refused=%d fd_path=%d inventory=%d alias=%d cross=%d entries=%d\n",
           ordinary_ok, symlink_refused, fd_path, available, alias_ok, cross, entries);
    close(rootfd); close(dirfd); close(aliasfd);
    return 0;
}
