# Local SMB adaptation

Upstream: [`github.com/jfjallid/go-smb`](https://github.com/jfjallid/go-smb),
MIT license, tag **v0.12.0**, commit
**f002be42e8ec6924a465fccf1713267c40f37e41**.

The original module path is retained. The root `go.mod` uses a local `replace`
so builds use these reviewed changes rather than an unpatched upstream release.
The upstream license must accompany redistributed source and binaries. No remote
fork, release, or upstream contribution is implied by this directory.

## Production changes

- **Opt-in concurrent reads.** `ServerConfig.ConcurrentReads` runs bounded
  read-containing frames without blocking the TCP receive loop. A compound
  frame keeps operation and reply order. Replies remain correlated by ordinary
  MessageID; this path does not emit an interim `STATUS_PENDING` or invent an
  AsyncID. READ and existing asynchronous-operation registries share a maximum
  of 64 outstanding operation IDs per connection. Admission reserves sequence
  numbers before a worker starts, and capacity errors reply to every compound
  member instead of leaving dependent requests waiting forever.
- **Scoped cancellation and teardown.** Each compound member gets its own child
  context. CANCEL is checked against both MessageID and session, cannot cancel
  a later unrelated member, and does not generate a second reply of its own.
  File/tree close interrupts matching operations; session/connection shutdown
  cancels and drains the owned frame workers. A closed tree rejects late handle
  registration, and a late file, pipe, or durable-handle result is closed rather
  than resurrecting a torn-down handle table. Session closing also prevents new
  tree creation and file opens. Explicit LOGOFF closes non-durable resources and
  retains its signing/encryption keys until its final reply has been flushed.
- **Compound protection correctness.** Related IDs select the owning session's
  key, but signatures are verified over the original wire bytes before inherited
  SessionID/TreeID/FileID values are substituted. Related substitution also runs
  after encrypted transport authentication. Inner encrypted session identities
  must agree with the authenticated transform. Encrypted requests are not exempt
  from MessageID replay checks. Established connections cannot renegotiate their
  connection-wide crypto state, and authenticated sessions cannot repeat their
  initial authentication exchange. Signed CANCEL remains usable during encrypted
  reads, independently of the target request.
- **Input and backend bounds.** `ServerConfig.MaxRequestSize` checks the framing
  header before allocating/reading a body. Zero preserves the upstream 24-bit
  protocol maximum; torrentfs configures 2 MiB. READ checks backend byte counts
  before slicing and maps context cancellation/deadlines to `STATUS_CANCELLED`
  and `STATUS_IO_TIMEOUT` rather than reporting an unrelated parameter error.
- **Stable filesystem identities for CIFS.** A requested CREATE `QFid` context
  returns the same nonzero VFS FileID as directory enumeration,
  `FILE_ALL_INFORMATION.IndexNumber`, and the now-supported standalone
  `FILE_INTERNAL_INFORMATION` query. The handle's persistent/volatile FileId is
  intentionally not repurposed as an inode. A metadata-only capture of the
  failing default-`serverino` Linux client showed omitted QFid replies followed
  by `FILE_INTERNAL_INFORMATION` returning `STATUS_NOT_SUPPORTED` and repeated
  `ESTALE` opens. The corrected default-serverino client has completed real open,
  large-offset hash, and shared-connection slow-read smoke checks; no
  `noserverino` workaround is used.
- **Concurrent client signing.** The upstream client's reusable keyed hash is
  mutex-protected: concurrent context cancellations issue signed CANCELs outside
  its ordinary send queue. This is exercised by the real-client cancellation
  regressions; it is not a new application authentication mechanism.
- **Concurrent CCM use.** The CCM AEAD keeps one CBC-MAC state per value. The
  server encrypts outbound PDUs from the receive loop and from concurrent read
  workers on one session, so `Seal` and `Open` now take turns behind a mutex.
  Without it the race detector reports overlapping `(*mac).Reset` and
  `(*mac).Write` calls, and a tag can be computed over another packet's state.
  Exercised by `TestConcurrentCCMUse` in `internal/smb`.
- **Short NTLM NEGOTIATE decoding.** Accept the 32-byte NEGOTIATE layout when a
  client omits the optional Version field instead of unconditionally requiring
  40 bytes. An observed Infuse request used this layout and previously received
  `STATUS_INVALID_PARAMETER`. A byte-level regression retains the request form
  without any authentication proof or password.
- **Named-pipe metadata queries.** Route pipe-handle QUERY_INFO requests to
  dedicated metadata responses rather than dereferencing IPC$'s absent VFS.
  Filesystem and security queries also handle shares without a VFS. The
  independent-client regression queries StandardInformation through SeekEnd and
  AllInformation through Stat before closing the pipe. Windows server-root
  browsing and media playback still require post-fix real-client validation.
- **Named-pipe DCERPC WRITE/READ transport.** The pipe backend implemented only
  `FSCTL_PIPE_TRANSCEIVE`, so any client carrying DCE/RPC with a separate SMB2
  WRITE and READ received `STATUS_NOT_SUPPORTED`. That is exactly what Apple's
  `smbx` client does when iOS Files connects to `smb://host` and enumerates
  shares (NEGOTIATE, NTLMv2, TREE_CONNECT `IPC$` and CREATE `srvsvc` succeed,
  then WRITE of the 72-byte no-auth RPC Bind is rejected and Files reports an
  input/output error); impacket behaves the same way, while `smbclient -L`
  uses IOCTL transceive and was unaffected. `dcerpc/server.PipeHandler` now
  keeps a per-open byte stream: WRITE buffers a PDU split across several
  WRITEs (or several back-to-back PDUs in one WRITE) and dispatches each
  complete PDU in stream order through the same dispatcher as Transceive,
  which is unchanged; READ drains the queued response bytes, returning at most
  the requested length and retaining the rest for the next READ. A READ issued
  before its WRITE blocks until the WRITE queues a response, the request
  context is cancelled (`STATUS_CANCELLED`), or the handle is closed
  (`STATUS_FILE_CLOSED`). Wire-level unit tests in `dcerpc/server` cover
  bind/request round trips, partial-PDU buffering, multi-PDU writes,
  fragmented READs, blocking/wakeup, cancellation, close and malformed
  `frag_length`. The independent-client regression in `internal/smb`
  (`TestIndependentClientPipeWriteReadShareEnumeration`) performs
  WRITE(Bind)/READ(BindAck)/WRITE(NetrShareEnumAll opnum 15 level 1)/
  READ(Response) over `IPC$\srvsvc` and asserts the virtual shares are
  returned.

The application registers only authenticated, read-only virtual shares and the
existing srvsvc share-enumeration implementation. It does not enable relay,
credential-capture, anonymous, guest, compression, or durable-handle features.
NTLMv2 proof verification, signing algorithms, and transport encryption remain
upstream implementations; the compatibility changes above adapt framing and
server request handling. This is a finite compatibility adaptation, not a claim
of complete Samba equivalence or a comprehensive protocol security audit.

## Verification

From the repository root:

```sh
go test -race ./internal/smb -count=1 -timeout=180s
go -C third_party/go-smb test -mod=readonly -race ./dcerpc/... ./smb/server ./smb -count=1 -timeout=180s
go -C third_party/go-smb test -mod=readonly -race ./ntlmssp ./spnego -count=1 -timeout=180s
```

Root tests perform actual NTLM handshakes, signed SMB2 and required-encryption
SMB3 reads, related CREATE/READ/CLOSE compounds, cancellation, file/tree/session
teardown, shared-connection saturation, framing limits, and independent-client
read-only/directory-enumeration checks. `TestIndependentClientPipeWriteReadShareEnumeration`
drives the DCERPC-over-pipe WRITE+READ transport (Bind then NetrShareEnumAll on
`IPC$\srvsvc`) the way Apple's smbx / impacket do; the in-module
`dcerpc/server` tests pin the same transport at the PDU level without a network
peer. Deterministic in-module wire tests inject an authenticated session to
pause a late CREATE result across tree/session teardown; those tests do not
substitute for the real authentication tests.

The upstream idle-deadline test now negotiates once and uses ECHO for later
keepalives: repeated NEGOTIATE on an established connection is no longer accepted
as a valid way to test idle-deadline renewal.
