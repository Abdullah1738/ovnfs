package smb

// NTSTATUS values follow MS-ERREF; warnings retain useful response data.
const (
	statusSuccess                uint32 = 0x00000000
	statusPending                uint32 = 0x00000103
	statusNotifyCleanup          uint32 = 0x0000010b
	statusNotifyEnumDir          uint32 = 0x0000010c
	statusObjectNameExists       uint32 = 0x40000000
	statusBufferOverflow         uint32 = 0x80000005
	statusNoMoreFiles            uint32 = 0x80000006
	statusNotImplemented         uint32 = 0xc0000002
	statusInvalidInfoClass       uint32 = 0xc0000003
	statusInfoLengthMismatch     uint32 = 0xc0000004
	statusInvalidHandle          uint32 = 0xc0000008
	statusInvalidParameter       uint32 = 0xc000000d
	statusNoSuchFile             uint32 = 0xc000000f
	statusInvalidDeviceRequest   uint32 = 0xc0000010
	statusEndOfFile              uint32 = 0xc0000011
	statusMoreProcessingRequired uint32 = 0xc0000016
	statusAccessDenied           uint32 = 0xc0000022
	statusBufferTooSmall         uint32 = 0xc0000023
	statusObjectNameInvalid      uint32 = 0xc0000033
	statusObjectNameNotFound     uint32 = 0xc0000034
	statusObjectNameCollision    uint32 = 0xc0000035
	statusObjectPathNotFound     uint32 = 0xc000003a
	statusSharingViolation       uint32 = 0xc0000043
	statusFileLockConflict       uint32 = 0xc0000054
	statusLockNotGranted         uint32 = 0xc0000055
	statusDeletePending          uint32 = 0xc0000056
	statusLogonFailure           uint32 = 0xc000006d
	statusRangeNotLocked         uint32 = 0xc000007e
	statusDiskFull               uint32 = 0xc000007f
	statusInsufficientResources  uint32 = 0xc000009a
	statusFileInvalid            uint32 = 0xc0000098
	statusFileIsADirectory       uint32 = 0xc00000ba
	statusNotSupported           uint32 = 0xc00000bb
	statusNetworkNameDeleted     uint32 = 0xc00000c9
	statusBadNetworkName         uint32 = 0xc00000cc
	statusRequestNotAccepted     uint32 = 0xc00000d0
	statusInternalError          uint32 = 0xc00000e5
	statusDirectoryNotEmpty      uint32 = 0xc0000101
	statusNotADirectory          uint32 = 0xc0000103
	statusCancelled              uint32 = 0xc0000120
	statusCannotDelete           uint32 = 0xc0000121
	statusFileClosed             uint32 = 0xc0000128
	statusUserSessionDeleted     uint32 = 0xc0000203
	statusInvalidDeviceState     uint32 = 0xc0000184
	statusNetworkSessionExpired  uint32 = 0xc000035c
	statusFsDriverRequired       uint32 = 0xc000019c
	statusNotAReparsePoint       uint32 = 0xc0000275
	statusIoReparseTagNotHandled uint32 = 0xc0000279
	statusTooManyLinks           uint32 = 0xc0000265
)
