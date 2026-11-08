package hostfile

// utimeOmit is <sys/stat.h>'s UTIME_OMIT, which golang.org/x/sys/unix lacks on darwin.
const utimeOmit = -2
