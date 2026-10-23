# Passive, SYSTEM-only, x64 Windows WTS collector. No commands or state-changing APIs.
# Load functions only: . .\probe.ps1 -LoadOnly
# Then: [LSW.ConnectTimeProbe.Native]::Snapshot() / ::Boot()
[CmdletBinding()]
param(
    [string]$OutputPath = '',
    [string]$StopPath = '',
    [ValidateRange(1, 1200)][int]$DurationSeconds = 1200,
    [ValidateRange(100, 1000)][int]$IntervalMilliseconds = 100,
    [switch]$LoadOnly
)
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

if (-not [Environment]::Is64BitProcess) { throw 'Use x64 pwsh: this probe asserts x64 Windows layouts.' }
if (-not ('LSW.ConnectTimeProbe.Native' -as [type])) {
    Add-Type -Language CSharp -TypeDefinition @'
using System;
using System.Collections.Generic;
using System.ComponentModel;
using System.Diagnostics;
using System.Globalization;
using System.Runtime.InteropServices;
using System.Text;

namespace LSW.ConnectTimeProbe {
    public sealed class ProbeError {
        public string api { get; set; }
        public int? win32_error { get; set; }
        public string message { get; set; }
    }
    public sealed class BootResult {
        public string boot_identifier { get; set; }
        public uint returned_bytes { get; set; }
        public string raw_base64 { get; set; }
        public string ntstatus { get; set; }
        public ProbeError error { get; set; }
    }
    public sealed class SessionReference {
        public uint session_id { get; set; }
        public string authentication_id { get; set; }
        public string boot_identifier { get; set; }
    }
    public sealed class SessionRow {
        public uint session_id { get; set; }
        public int enumerated_state { get; set; }
        public string station { get; set; }
        public string user { get; set; }
        public string domain { get; set; }
        public int? state { get; set; }
        public int? session_flags { get; set; }
        public string info_user { get; set; }
        public string info_domain { get; set; }
        public string client_name { get; set; }
        public string logon_time { get; set; }
        public string connect_time { get; set; }
        public string disconnect_time { get; set; }
        public string last_input_time { get; set; }
        public string current_time { get; set; }
        public string sid { get; set; }
        public string authentication_id { get; set; }
        public SessionReference session_ref { get; set; }
        public uint info_returned_bytes { get; set; }
        public string info_raw_base64 { get; set; }
        public string note { get; set; }
        public List<ProbeError> errors { get; set; } = new List<ProbeError>();
    }
    public sealed class SnapshotResult {
        public bool complete { get; set; } = true;
        public BootResult boot { get; set; }
        public long query_elapsed_ms { get; set; }
        public uint enumerated_count { get; set; }
        public List<SessionRow> sessions { get; set; } = new List<SessionRow>();
        public List<ProbeError> errors { get; set; } = new List<ProbeError>();
    }
    public static class Native {
        const int MaxNativeBytes = 1024 * 1024;
        const uint MaxEnumerated = 1024;
        const int MaxCollected = 64;
        const int InfoExSize = 232;
        [StructLayout(LayoutKind.Sequential)]
        struct WTS_SESSION_INFOW {
            public uint SessionId;
            public IntPtr WinStationName;
            public int State;
        }
        [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode, Pack = 8)]
        struct WTSINFOEX_LEVEL1_W {
            public uint SessionId;
            public int SessionState;
            public int SessionFlags;
            [MarshalAs(UnmanagedType.ByValTStr, SizeConst = 33)] public string WinStationName;
            [MarshalAs(UnmanagedType.ByValTStr, SizeConst = 21)] public string UserName;
            [MarshalAs(UnmanagedType.ByValTStr, SizeConst = 18)] public string DomainName;
            public long LogonTime;
            public long ConnectTime;
            public long DisconnectTime;
            public long LastInputTime;
            public long CurrentTime;
            public uint IncomingBytes, OutgoingBytes, IncomingFrames, OutgoingFrames;
            public uint IncomingCompressedBytes, OutgoingCompressedBytes;
        }
        [StructLayout(LayoutKind.Sequential, Pack = 8)]
        struct WTSINFOEXW {
            public uint Level;
            public WTSINFOEX_LEVEL1_W Data;
        }
        [StructLayout(LayoutKind.Sequential)]
        struct LUID { public uint LowPart; public int HighPart; }
        [StructLayout(LayoutKind.Sequential, Pack = 8)]
        struct TOKEN_STATISTICS {
            public LUID TokenId, AuthenticationId;
            public long ExpirationTime;
            public int TokenType, ImpersonationLevel;
            public uint DynamicCharged, DynamicAvailable, GroupCount, PrivilegeCount;
            public LUID ModifiedId;
        }
        [StructLayout(LayoutKind.Sequential, Pack = 8)]
        struct BOOT_ENVIRONMENT_INFORMATION {
            public Guid BootIdentifier;
            public uint FirmwareType;
            public ulong BootFlags;
        }
        [DllImport("wtsapi32.dll", SetLastError = true)]
        [return: MarshalAs(UnmanagedType.Bool)]
        static extern bool WTSEnumerateSessionsW(IntPtr server, uint reserved, uint version, out IntPtr sessions, out uint count);
        [DllImport("wtsapi32.dll", SetLastError = true)]
        [return: MarshalAs(UnmanagedType.Bool)]
        static extern bool WTSQuerySessionInformationW(IntPtr server, uint id, int infoClass, out IntPtr buffer, out uint bytes);
        [DllImport("wtsapi32.dll")]
        static extern void WTSFreeMemory(IntPtr memory);
        [DllImport("wtsapi32.dll", SetLastError = true)]
        [return: MarshalAs(UnmanagedType.Bool)]
        static extern bool WTSQueryUserToken(uint id, out IntPtr token);
        [DllImport("advapi32.dll", SetLastError = true)]
        [return: MarshalAs(UnmanagedType.Bool)]
        static extern bool GetTokenInformation(IntPtr token, int tokenClass, IntPtr info, uint length, out uint returned);
        [DllImport("advapi32.dll", SetLastError = true)]
        [return: MarshalAs(UnmanagedType.Bool)]
        static extern bool ConvertSidToStringSidW(IntPtr sid, out IntPtr text);
        [DllImport("kernel32.dll")]
        static extern IntPtr LocalFree(IntPtr memory);
        [DllImport("kernel32.dll", SetLastError = true)]
        [return: MarshalAs(UnmanagedType.Bool)]
        static extern bool CloseHandle(IntPtr handle);
        [DllImport("ntdll.dll")]
        static extern int NtQuerySystemInformation(int infoClass, IntPtr info, uint length, out uint returned);

        static ProbeError Error(string api, int code) {
            return new ProbeError { api = api, win32_error = code, message = new Win32Exception(code).Message };
        }
        static ProbeError Invalid(string api, string message) {
            return new ProbeError { api = api, message = message };
        }
        static long Offset(Type type, string field) { return Marshal.OffsetOf(type, field).ToInt64(); }
        public static void AssertLayout() {
            if (IntPtr.Size != 8 || !BitConverter.IsLittleEndian) throw new InvalidOperationException("x64 little-endian Windows required");
            if (Marshal.SizeOf(typeof(WTS_SESSION_INFOW)) != 24 || Offset(typeof(WTS_SESSION_INFOW), "WinStationName") != 8 || Offset(typeof(WTS_SESSION_INFOW), "State") != 16)
                throw new InvalidOperationException("WTS_SESSION_INFOW layout mismatch");
            if (Marshal.SizeOf(typeof(WTSINFOEX_LEVEL1_W)) != 224 || Offset(typeof(WTSINFOEX_LEVEL1_W), "LogonTime") != 160 || Offset(typeof(WTSINFOEX_LEVEL1_W), "ConnectTime") != 168 || Offset(typeof(WTSINFOEX_LEVEL1_W), "DisconnectTime") != 176)
                throw new InvalidOperationException("WTSINFOEX_LEVEL1_W layout mismatch");
            if (Marshal.SizeOf(typeof(WTSINFOEXW)) != InfoExSize || Offset(typeof(WTSINFOEXW), "Data") != 8)
                throw new InvalidOperationException("WTSINFOEXW layout mismatch");
            if (Marshal.SizeOf(typeof(TOKEN_STATISTICS)) != 56 || Offset(typeof(TOKEN_STATISTICS), "AuthenticationId") != 8)
                throw new InvalidOperationException("TOKEN_STATISTICS layout mismatch");
            if (Marshal.SizeOf(typeof(BOOT_ENVIRONMENT_INFORMATION)) != 32 || Offset(typeof(BOOT_ENVIRONMENT_INFORMATION), "BootFlags") != 24)
                throw new InvalidOperationException("BOOT_ENVIRONMENT_INFORMATION layout mismatch");
        }
        static void CheckBuffer(IntPtr p, uint bytes, int minimum, int maximum, string api) {
            if (p == IntPtr.Zero || bytes < minimum || bytes > maximum) throw new InvalidOperationException(api + " invalid buffer pointer/length " + bytes);
            checked { long end = p.ToInt64() + (long)bytes; if (end < p.ToInt64()) throw new InvalidOperationException(api + " pointer overflow"); }
        }
        static string Unicode(byte[] data, int offset, int characters) {
            int count = 0;
            for (; count < characters; count++) if (data[offset + count * 2] == 0 && data[offset + count * 2 + 1] == 0) break;
            return Encoding.Unicode.GetString(data, offset, count * 2);
        }
        static string Time(byte[] data, int offset) { return BitConverter.ToInt64(data, offset).ToString(CultureInfo.InvariantCulture); }
        static string QueryText(uint id, int infoClass, string label, List<ProbeError> errors) {
            IntPtr p = IntPtr.Zero;
            try {
                uint bytes;
                if (!WTSQuerySessionInformationW(IntPtr.Zero, id, infoClass, out p, out bytes)) {
                    errors.Add(Error(label, Marshal.GetLastWin32Error())); return null;
                }
                CheckBuffer(p, bytes, 2, MaxNativeBytes, label);
                if ((bytes & 1) != 0) throw new InvalidOperationException("odd UTF-16 byte length");
                byte[] data = new byte[checked((int)bytes)];
                Marshal.Copy(p, data, 0, data.Length);
                return Unicode(data, 0, data.Length / 2);
            } catch (Exception e) { errors.Add(Invalid(label, e.Message)); return null; }
            finally { if (p != IntPtr.Zero) WTSFreeMemory(p); }
        }
        public static BootResult Boot() {
            AssertLayout();
            var result = new BootResult();
            IntPtr p = Marshal.AllocHGlobal(32);
            try {
                Marshal.Copy(new byte[32], 0, p, 32);
                uint returned;
                int status = NtQuerySystemInformation(90, p, 32, out returned);
                result.ntstatus = "0x" + unchecked((uint)status).ToString("X8", CultureInfo.InvariantCulture);
                result.returned_bytes = returned;
                if (status < 0 || returned < 16 || returned > 32) {
                    result.error = Invalid("NtQuerySystemInformation(90)", "status=" + result.ntstatus + ", returned=" + returned);
                    return result;
                }
                byte[] raw = new byte[checked((int)returned)];
                Marshal.Copy(p, raw, 0, raw.Length);
                result.raw_base64 = Convert.ToBase64String(raw);
                byte[] guid = new byte[16]; Array.Copy(raw, guid, 16);
                result.boot_identifier = new Guid(guid).ToString("D");
                return result;
            } catch (Exception e) { result.error = Invalid("Boot", e.Message); return result; }
            finally { Marshal.FreeHGlobal(p); }
        }
        static void Info(SessionRow row) {
            IntPtr p = IntPtr.Zero;
            try {
                uint bytes;
                if (!WTSQuerySessionInformationW(IntPtr.Zero, row.session_id, 25, out p, out bytes)) {
                    row.errors.Add(Error("WTSSessionInfoEx", Marshal.GetLastWin32Error())); return;
                }
                CheckBuffer(p, bytes, InfoExSize, 4096, "WTSSessionInfoEx");
                row.info_returned_bytes = bytes;
                byte[] raw = new byte[checked((int)bytes)];
                Marshal.Copy(p, raw, 0, raw.Length);
                row.info_raw_base64 = Convert.ToBase64String(raw);
                if (BitConverter.ToUInt32(raw, 0) != 1) throw new InvalidOperationException("WTSSessionInfoEx Level != 1");
                if (BitConverter.ToUInt32(raw, 8) != row.session_id) throw new InvalidOperationException("WTSSessionInfoEx SessionId mismatch");
                row.state = BitConverter.ToInt32(raw, 12);
                row.session_flags = BitConverter.ToInt32(raw, 16);
                row.info_user = Unicode(raw, 86, 21);
                row.info_domain = Unicode(raw, 128, 18);
                row.logon_time = Time(raw, 168);
                row.connect_time = Time(raw, 176);
                row.disconnect_time = Time(raw, 184);
                row.last_input_time = Time(raw, 192);
                row.current_time = Time(raw, 200);
            } catch (Exception e) { row.errors.Add(Invalid("WTSSessionInfoEx", e.Message)); }
            finally { if (p != IntPtr.Zero) WTSFreeMemory(p); }
        }
        static void Identity(SessionRow row) {
            IntPtr token = IntPtr.Zero, buffer = IntPtr.Zero;
            try {
                if (!WTSQueryUserToken(row.session_id, out token)) {
                    row.errors.Add(Error("WTSQueryUserToken", Marshal.GetLastWin32Error())); return;
                }
                uint needed;
                GetTokenInformation(token, 1, IntPtr.Zero, 0, out needed);
                if (needed < 16 || needed > MaxNativeBytes) throw new InvalidOperationException("TokenUser invalid required length " + needed);
                buffer = Marshal.AllocHGlobal(checked((int)needed));
                uint returned;
                if (!GetTokenInformation(token, 1, buffer, needed, out returned)) {
                    row.errors.Add(Error("GetTokenInformation(TokenUser)", Marshal.GetLastWin32Error())); return;
                }
                CheckBuffer(buffer, returned, 16, checked((int)needed), "TokenUser");
                IntPtr sid = Marshal.ReadIntPtr(buffer, 0);
                long start = buffer.ToInt64(), address = sid.ToInt64(), end = checked(start + returned);
                if (address < start || address > end - 8) throw new InvalidOperationException("TokenUser SID pointer outside returned buffer");
                int subAuthorities = Marshal.ReadByte(sid, 1);
                if (subAuthorities > 15 || checked(address + 8 + 4L * subAuthorities) > end) throw new InvalidOperationException("TokenUser SID length outside returned buffer");
                IntPtr text = IntPtr.Zero;
                try {
                    if (!ConvertSidToStringSidW(sid, out text)) row.errors.Add(Error("ConvertSidToStringSidW", Marshal.GetLastWin32Error()));
                    else if (text == IntPtr.Zero) throw new InvalidOperationException("empty converted SID pointer");
                    else row.sid = Marshal.PtrToStringUni(text);
                } finally { if (text != IntPtr.Zero) LocalFree(text); }
                Marshal.FreeHGlobal(buffer); buffer = IntPtr.Zero;
                buffer = Marshal.AllocHGlobal(56);
                if (!GetTokenInformation(token, 10, buffer, 56, out returned)) {
                    row.errors.Add(Error("GetTokenInformation(TokenStatistics)", Marshal.GetLastWin32Error())); return;
                }
                CheckBuffer(buffer, returned, 56, 56, "TokenStatistics");
                uint low = unchecked((uint)Marshal.ReadInt32(buffer, 8));
                uint high = unchecked((uint)Marshal.ReadInt32(buffer, 12));
                row.authentication_id = high.ToString("X8", CultureInfo.InvariantCulture) + low.ToString("X8", CultureInfo.InvariantCulture);
            } catch (Exception e) { row.errors.Add(Invalid("Identity", e.Message)); }
            finally { if (buffer != IntPtr.Zero) Marshal.FreeHGlobal(buffer); if (token != IntPtr.Zero) CloseHandle(token); }
        }
        public static SnapshotResult Snapshot() {
            AssertLayout();
            var timer = Stopwatch.StartNew();
            var result = new SnapshotResult { boot = Boot() };
            if (result.boot.error != null) result.complete = false;
            IntPtr p = IntPtr.Zero;
            try {
                uint count;
                if (!WTSEnumerateSessionsW(IntPtr.Zero, 0, 1, out p, out count)) {
                    result.errors.Add(Error("WTSEnumerateSessionsW", Marshal.GetLastWin32Error())); result.complete = false; return result;
                }
                result.enumerated_count = count;
                if (count > MaxEnumerated) throw new InvalidOperationException("enumeration limit exceeded: " + count);
                if (count > 0) CheckBuffer(p, checked(count * 24), 24, checked((int)MaxEnumerated * 24), "WTSEnumerateSessionsW");
                for (uint i = 0; i < count; i++) {
                    IntPtr entry = IntPtr.Add(p, checked((int)i * 24));
                    var native = (WTS_SESSION_INFOW)Marshal.PtrToStructure(entry, typeof(WTS_SESSION_INFOW));
                    if (native.SessionId == 0 || native.SessionId >= 65536 || (native.State != 0 && native.State != 4)) continue;
                    if (result.sessions.Count >= MaxCollected) throw new InvalidOperationException("collected-row limit exceeded: " + MaxCollected);
                    var row = new SessionRow { session_id = native.SessionId, enumerated_state = native.State };
                    // The station name is unnecessary; don't dereference an unbounded nested pointer.
                    row.station = null;
                    row.user = QueryText(native.SessionId, 5, "WTSUserName", row.errors);
                    row.domain = QueryText(native.SessionId, 7, "WTSDomainName", row.errors);
                    if (String.IsNullOrEmpty(row.user)) {
                        row.note = "unbound/empty user: no raw INFOEX or token query";
                        row.state = native.State;
                    } else {
                        Info(row);
                        row.client_name = QueryText(native.SessionId, 10, "WTSClientName", row.errors);
                        Identity(row);
                        if (!String.IsNullOrEmpty(row.sid) && !String.IsNullOrEmpty(row.authentication_id) && !String.IsNullOrEmpty(result.boot.boot_identifier))
                            row.session_ref = new SessionReference { session_id = row.session_id, authentication_id = row.authentication_id, boot_identifier = result.boot.boot_identifier };
                    }
                    if (row.errors.Count > 0) result.complete = false;
                    result.sessions.Add(row);
                }
            } catch (Exception e) { result.errors.Add(Invalid("Snapshot", e.Message)); result.complete = false; }
            finally { if (p != IntPtr.Zero) WTSFreeMemory(p); result.query_elapsed_ms = timer.ElapsedMilliseconds; }
            return result;
        }
    }
}
'@
}
[LSW.ConnectTimeProbe.Native]::AssertLayout()
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
try {
    $collectorSid = $identity.User.Value
    $collectorName = $identity.Name
} finally { $identity.Dispose() }
if ($collectorSid -ne 'S-1-5-18') { throw "Run as SYSTEM; actual collector SID is $collectorSid" }
if ($LoadOnly) { return }
if ([string]::IsNullOrWhiteSpace($OutputPath)) { throw 'OutputPath is required unless LoadOnly is specified.' }
if (-not [IO.Path]::IsPathRooted($OutputPath)) { throw 'OutputPath must be an absolute filesystem path.' }
if ($StopPath -and -not [IO.Path]::IsPathRooted($StopPath)) { throw 'StopPath must be an absolute filesystem path.' }
$OutputPath = [IO.Path]::GetFullPath($OutputPath)
$parentDirectory = [IO.Path]::GetDirectoryName($OutputPath)
[void][IO.Directory]::CreateDirectory($parentDirectory)
# Never overwrite or append to an old experiment's data. Frames are flushed to
# disk immediately; no trace history is retained in memory.
$stream = [IO.FileStream]::new($OutputPath, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::ReadWrite)
$writer = [IO.StreamWriter]::new($stream, [Text.UTF8Encoding]::new($false))
$writer.AutoFlush = $true
$clock = [Diagnostics.Stopwatch]::StartNew()
$sample = 0L
$stopReason = 'duration'
$collector = [ordered]@{
    pid = $PID
    session_id = [Diagnostics.Process]::GetCurrentProcess().SessionId
    name = $collectorName
    sid = $collectorSid
    pointer_bytes = [IntPtr]::Size
    stopwatch_frequency = [Diagnostics.Stopwatch]::Frequency.ToString([Globalization.CultureInfo]::InvariantCulture)
    duration_seconds = $DurationSeconds
    interval_ms = $IntervalMilliseconds
}
try {
    while ($clock.Elapsed.TotalSeconds -lt $DurationSeconds) {
        if ($StopPath -and [IO.File]::Exists($StopPath)) { $stopReason = 'stop_file'; break }
        $snapshot = [LSW.ConnectTimeProbe.Native]::Snapshot()
        $frame = [ordered]@{
            schema = 'lsw-connecttime-raw-v1'
            kind = 'snapshot'
            sequence = $sample
            collector_utc = [DateTime]::UtcNow.ToString('O')
            elapsed_ticks = $clock.ElapsedTicks.ToString([Globalization.CultureInfo]::InvariantCulture)
            elapsed_ms = $clock.ElapsedMilliseconds
            collector = $collector
            snapshot = $snapshot
        }
        $writer.WriteLine(($frame | ConvertTo-Json -Depth 12 -Compress))
        $sample++
        if ($StopPath -and [IO.File]::Exists($StopPath)) { $stopReason = 'stop_file'; break }
        # Queries have no generally safe cancellation API. This loop bounds new
        # samples, not a stalled native WTS call; main owns the temporary task.
        $remainingMs = [Math]::Floor(($DurationSeconds - $clock.Elapsed.TotalSeconds) * 1000)
        if ($remainingMs -gt 0) { Start-Sleep -Milliseconds ([int][Math]::Min($IntervalMilliseconds, $remainingMs)) }
    }
    $writer.WriteLine((([ordered]@{
        schema = 'lsw-connecttime-raw-v1'
        kind = 'complete'
        collector_utc = [DateTime]::UtcNow.ToString('O')
        elapsed_ms = $clock.ElapsedMilliseconds
        sample_count = $sample
        stop_reason = $stopReason
        collector = $collector
    }) | ConvertTo-Json -Depth 5 -Compress))
} finally {
    $clock.Stop()
    $writer.Dispose()
}
