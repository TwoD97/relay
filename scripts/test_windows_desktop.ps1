<#
Real native Windows/WebView2 smoke test. Requires a packaged/extracted Relay desktop,
a matching Microsoft-signed msedgedriver.exe, and a disposable Linux SSH fixture.
The application and controller run as native Windows PE processes; the test
does not invoke WSL. Fixture provisioning/teardown belongs to the caller.
LaunchOnly uses no SSH fixture, mouse/keyboard input, or clipboard, and checks
the extracted folder with its own state and WebView2 profile.

Use only disposable fixture credentials. No provider login or inference occurs.
Example:
  .\test_windows_desktop.ps1 -Desktop C:\Path\relay-desktop.exe `
    -Bundle C:\Path\runtime -Driver C:\Tools\msedgedriver.exe `
    -FixtureTarget relay@127.0.0.1 -FixturePort 22022 `
    -FixtureFingerprint SHA256:... -FixturePasswordFile C:\Temp\fixture-password
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory=$true)][string]$Desktop,
    [Parameter(Mandatory=$true)][string]$Bundle,
    [Parameter(Mandatory=$true)][string]$Driver,
    [string]$FixtureTarget,
    [ValidateRange(1,65535)][int]$FixturePort = 22,
    [string]$FixtureFingerprint,
    [string]$FixturePasswordFile,
    [string]$FixtureDirectory = '/home/relay',
    [string]$Artifacts = '',
    [switch]$OverrideBundle,
    [switch]$FrameOnly,
    # Isolated native launch/default resources only; no SSH, mouse, keys or clipboard.
    [switch]$LaunchOnly,
    # SSH/PTY rendering and resize checks without physical mouse or clipboard.
    [switch]$RenderingOnly,
    # Enable only with a disposable account clipboard. This replaces clipboard
    # contents with fixture text; private clipboard contents are never captured.
    [switch]$Clipboard
)

$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Net.Http
Add-Type -AssemblyName System.Drawing
Add-Type @'
using System;
using System.Runtime.InteropServices;
using System.Text;
public static class RelayWindowProbe {
  [StructLayout(LayoutKind.Sequential)] public struct Rect { public int Left, Top, Right, Bottom; }
  [StructLayout(LayoutKind.Sequential)] public struct Point { public int X, Y; }
  [DllImport("user32.dll")] public static extern bool ClientToScreen(IntPtr hwnd, ref Point point);
  [DllImport("user32.dll")] public static extern IntPtr SendMessageTimeout(IntPtr hwnd, uint message, IntPtr w, IntPtr l, uint flags, uint timeout, out IntPtr result);
  [DllImport("user32.dll")] public static extern bool GetWindowRect(IntPtr hwnd, out Rect rect);
  [DllImport("user32.dll")] public static extern bool GetClientRect(IntPtr hwnd, out Rect rect);
  [DllImport("user32.dll")] public static extern uint GetDpiForWindow(IntPtr hwnd);
  [DllImport("user32.dll")] public static extern IntPtr GetParent(IntPtr hwnd);
  [DllImport("user32.dll")] public static extern uint GetWindowThreadProcessId(IntPtr hwnd, out uint pid);
  [DllImport("user32.dll")] public static extern bool PrintWindow(IntPtr hwnd, IntPtr hdc, uint flags);
  public delegate bool WindowCallback(IntPtr hwnd, IntPtr parameter);
  [DllImport("user32.dll")] public static extern bool EnumWindows(WindowCallback callback, IntPtr parameter);
  [DllImport("user32.dll")] public static extern bool EnumChildWindows(IntPtr parent, WindowCallback callback, IntPtr parameter);
  [DllImport("user32.dll")] public static extern bool IsWindowVisible(IntPtr hwnd);
  [DllImport("user32.dll")] public static extern bool IsZoomed(IntPtr hwnd);
  [DllImport("user32.dll")] public static extern bool IsIconic(IntPtr hwnd);
  [DllImport("user32.dll")] public static extern bool ShowWindow(IntPtr hwnd, int command);
  [DllImport("user32.dll")] public static extern bool SetForegroundWindow(IntPtr hwnd);
  [DllImport("user32.dll")] public static extern IntPtr GetForegroundWindow();
  [DllImport("user32.dll")] public static extern IntPtr SetThreadDpiAwarenessContext(IntPtr context);
  [DllImport("user32.dll")] public static extern bool GetCursorPos(out Point point);
  [DllImport("user32.dll")] public static extern bool SetCursorPos(int x, int y);
  [DllImport("user32.dll")] public static extern IntPtr WindowFromPoint(Point point);
  [DllImport("user32.dll")] public static extern IntPtr GetAncestor(IntPtr hwnd, uint flags);
  [StructLayout(LayoutKind.Sequential)] public struct MouseInput { public int X, Y; public uint Data, Flags, Time; public UIntPtr Extra; }
  [StructLayout(LayoutKind.Sequential)] public struct Input { public uint Type; public MouseInput Mouse; }
  [DllImport("user32.dll")] public static extern int GetSystemMetrics(int index);
  [DllImport("user32.dll", SetLastError=true)] public static extern uint SendInput(uint count, Input[] inputs, int size);
  public static bool ClickAt(int x, int y) {
    int left=GetSystemMetrics(76), top=GetSystemMetrics(77), width=GetSystemMetrics(78), height=GetSystemMetrics(79);
    if(width<2 || height<2) return false;
    var inputs=new Input[3];
    inputs[0].Mouse.X=(int)Math.Round((x-left)*65535.0/(width-1));
    inputs[0].Mouse.Y=(int)Math.Round((y-top)*65535.0/(height-1));
    inputs[0].Mouse.Flags=0xC001;
    inputs[1].Mouse.Flags=0x0002;
    inputs[2].Mouse.Flags=0x0004;
    return SendInput(3,inputs,Marshal.SizeOf(typeof(Input)))==3;
  }
  [DllImport("user32.dll")] public static extern int GetWindowLong(IntPtr hwnd, int index);
  [DllImport("dwmapi.dll")] public static extern int DwmGetWindowAttribute(IntPtr hwnd, uint attribute, out int value, int size);
  [DllImport("user32.dll")] public static extern bool SetWindowPos(IntPtr hwnd, IntPtr after, int x, int y, int width, int height, uint flags);
  [DllImport("user32.dll", CharSet=CharSet.Unicode)] public static extern int GetClassName(IntPtr hwnd, StringBuilder name, int count);
  public sealed class Geometry {
    public int Width, Height;
    public uint DPI;
    public Rect[] Views;
  }
  public static Geometry FrameGeometry(IntPtr hwnd) {
    // Query physical pixels without changing the user's monitor settings.
    IntPtr previous = SetThreadDpiAwarenessContext(new IntPtr(-4));
    try {
      Rect client; Point origin = new Point();
      if (!GetClientRect(hwnd, out client) || !ClientToScreen(hwnd, ref origin)) return null;
      var views = new System.Collections.Generic.List<Rect>();
      EnumChildWindows(hwnd, (child, ignored) => {
        var name = new StringBuilder(256); GetClassName(child, name, name.Capacity);
        Rect bounds;
        if (GetParent(child) == hwnd && name.ToString() == "WRY_WEBVIEW" && GetWindowRect(child, out bounds)) {
          bounds.Left -= origin.X; bounds.Right -= origin.X;
          bounds.Top -= origin.Y; bounds.Bottom -= origin.Y;
          views.Add(bounds);
        }
        return true;
      }, IntPtr.Zero);
      return new Geometry { Width=client.Right-client.Left, Height=client.Bottom-client.Top, DPI=GetDpiForWindow(hwnd), Views=views.ToArray() };
    } finally { if (previous != IntPtr.Zero) SetThreadDpiAwarenessContext(previous); }
  }
  public static IntPtr MainFrame(int processId) {
    IntPtr found = IntPtr.Zero;
    EnumWindows((window, parameter) => {
      uint owner; GetWindowThreadProcessId(window, out owner);
      Rect bounds;
      if (owner == processId && IsWindowVisible(window) && GetWindowRect(window, out bounds) &&
          bounds.Right-bounds.Left >= 390 && bounds.Bottom-bounds.Top >= 500) {
        found = window; return false;
      }
      return true;
    }, IntPtr.Zero);
    return found;
  }
  public static string[] OwnedWindows(int processId) {
    var found = new System.Collections.Generic.List<string>();
    EnumWindows((window, parameter) => {
      uint owner; GetWindowThreadProcessId(window, out owner);
      Rect bounds;
      if (owner == processId && GetWindowRect(window, out bounds)) {
        var name = new StringBuilder(256); GetClassName(window, name, name.Capacity);
        found.Add(String.Format("{0}: visible={1} minimized={2} rect={3},{4},{5},{6} class={7}",
          window, IsWindowVisible(window), IsIconic(window), bounds.Left, bounds.Top, bounds.Right, bounds.Bottom, name));
      }
      return true;
    }, IntPtr.Zero);
    return found.ToArray();
  }
  public static int VisibleConsoles(int[] processIds) {
    int count = 0;
    EnumWindows((window, parameter) => {
      uint owner; GetWindowThreadProcessId(window, out owner);
      if (Array.IndexOf(processIds, (int)owner) >= 0 && IsWindowVisible(window)) {
        var name = new StringBuilder(256); GetClassName(window, name, name.Capacity);
        if (name.ToString() == "ConsoleWindowClass") count++;
      }
      return true;
    }, IntPtr.Zero);
    return count;
  }
}
'@

function Assert-True($Condition, [string]$Message) {
    if (-not $Condition) { throw $Message }
}

function Wait-Until([scriptblock]$Predicate, [string]$Description, [int]$Seconds = 30) {
    $deadline = [DateTime]::UtcNow.AddSeconds($Seconds)
    while ([DateTime]::UtcNow -lt $deadline) {
        $result = & $Predicate
        if ($result) { return $result }
        Start-Sleep -Milliseconds 100
    }
    throw "Timed out: $Description"
}

function Quote-Argument([string]$Value) {
    # Windows CommandLineToArgvW/MSVC quoting, including quotes and trailing '\'.
    $builder = New-Object System.Text.StringBuilder
    [void]$builder.Append('"')
    $slashes = 0
    foreach ($character in $Value.ToCharArray()) {
        if ($character -eq [char]92) { $slashes++; continue }
        if ($character -eq [char]34) {
            [void]$builder.Append(('\' * ($slashes * 2 + 1)))
        } else {
            [void]$builder.Append(('\' * $slashes))
        }
        [void]$builder.Append($character)
        $slashes = 0
    }
    [void]$builder.Append(('\' * ($slashes * 2)))
    [void]$builder.Append('"')
    return $builder.ToString()
}

function Start-OwnedProcess([string]$Executable, [string[]]$Arguments, [hashtable]$Environment) {
    $info = New-Object System.Diagnostics.ProcessStartInfo
    $info.FileName = $Executable
    $info.Arguments = (($Arguments | ForEach-Object { Quote-Argument $_ }) -join ' ')
    $info.UseShellExecute = $false
    $info.CreateNoWindow = $true
    $info.RedirectStandardOutput = $true
    $info.RedirectStandardError = $true
    $info.StandardOutputEncoding = New-Object System.Text.UTF8Encoding($false)
    $info.StandardErrorEncoding = New-Object System.Text.UTF8Encoding($false)
    foreach ($key in $Environment.Keys) {
        if ($null -eq $Environment[$key]) { $info.EnvironmentVariables.Remove($key) }
        else { $info.EnvironmentVariables[$key] = $Environment[$key] }
    }
    $process = New-Object System.Diagnostics.Process
    $process.StartInfo = $info
    Assert-True ($process.Start()) 'Could not start test-owned process'
    return [pscustomobject]@{ Process=$process; Output=$process.StandardOutput.ReadToEndAsync(); Error=$process.StandardError.ReadToEndAsync() }
}

function Stop-OwnedProcess($Owned) {
    if ($null -eq $Owned) { return }
    if (-not $Owned.Process.HasExited) {
        $Owned.Process.Kill()
        [void]$Owned.Process.WaitForExit(5000)
    }
    $Owned.Process.Dispose()
}

function Process-Identity([int]$ProcessID, [string]$ExpectedPath) {
    $process = Get-Process -Id $ProcessID -ErrorAction Stop
    Assert-True ([string]::Equals($process.Path, $ExpectedPath, [StringComparison]::OrdinalIgnoreCase)) 'Unexpected executable for test-owned process'
    return [pscustomobject]@{ Id=$ProcessID; Path=$process.Path; Started=$process.StartTime.ToUniversalTime().Ticks }
}

function Controller-Identity([int]$ProcessID) {
    $process = Get-Process -Id $ProcessID -ErrorAction Stop
    $releaseRoot = (Join-Path $script:StateDirectory 'controller-releases') + '\'
    Assert-True ([string]::Equals($process.Path, $script:Controller, [StringComparison]::OrdinalIgnoreCase) -or
        $process.Path.StartsWith($releaseRoot, [StringComparison]::OrdinalIgnoreCase)) 'Native controller ran outside its bundle or private release directory'
    Assert-True ((Get-FileHash -LiteralPath $process.Path -Algorithm SHA256).Hash -eq
        (Get-FileHash -LiteralPath $script:Controller -Algorithm SHA256).Hash) 'Running native controller differs from the packaged executable'
    return Process-Identity $ProcessID $process.Path
}

function Stop-Identity($Identity) {
    if ($null -eq $Identity) { return }
    $process = Get-Process -Id $Identity.Id -ErrorAction SilentlyContinue
    if ($null -ne $process -and $process.StartTime.ToUniversalTime().Ticks -eq $Identity.Started -and
        [string]::Equals($process.Path, $Identity.Path, [StringComparison]::OrdinalIgnoreCase)) {
        $process.Kill()
        [void]$process.WaitForExit(5000)
    }
}

function New-HTTPClient([bool]$Cookies = $false) {
    $handler = New-Object System.Net.Http.HttpClientHandler
    $handler.UseProxy = $false
    $handler.UseCookies = $Cookies
    $handler.AllowAutoRedirect = $true
    if ($Cookies) { $handler.CookieContainer = New-Object System.Net.CookieContainer }
    $client = New-Object System.Net.Http.HttpClient($handler)
    $client.Timeout = [TimeSpan]::FromSeconds(75)
    return $client
}

function HTTP($Client, [string]$Method, [string]$URL, $Body = $null, [hashtable]$Headers = @{}) {
    $request = New-Object System.Net.Http.HttpRequestMessage((New-Object System.Net.Http.HttpMethod($Method)), $URL)
    foreach ($key in $Headers.Keys) { [void]$request.Headers.TryAddWithoutValidation($key, [string]$Headers[$key]) }
    if ($null -ne $Body) {
        $request.Content = New-Object System.Net.Http.StringContent(($Body | ConvertTo-Json -Depth 20 -Compress), [Text.Encoding]::UTF8, 'application/json')
    }
    try {
        $response = $Client.SendAsync($request).GetAwaiter().GetResult()
        try {
            return [pscustomobject]@{ Status=[int]$response.StatusCode; Body=$response.Content.ReadAsStringAsync().GetAwaiter().GetResult() }
        } finally { $response.Dispose() }
    } finally { $request.Dispose() }
}

function WD([string]$Method, [string]$Path, $Body = $null) {
    $response = HTTP $script:DriverHTTP $Method ($script:DriverURL + $Path) $Body
    $decoded = $response.Body | ConvertFrom-Json
    if ($decoded.value.PSObject.Properties.Name -contains 'error') {
        $message = [regex]::Replace([string]$decoded.value.message, 'token=[^\s&"'']+', 'token=[redacted]')
        throw "WebDriver $($decoded.value.error): $message"
    }
    return $decoded.value
}

function Session-WD([string]$Method, [string]$Path, $Body = $null) {
    return WD $Method ("/session/" + $script:SessionID + $Path) $Body
}

function Execute-JS([string]$Source, [object[]]$Arguments = @()) {
    return Session-WD POST '/execute/sync' @{ script=$Source; args=$Arguments }
}

function Execute-AsyncJS([string]$Source, [object[]]$Arguments = @()) {
    return Session-WD POST '/execute/async' @{ script=$Source; args=$Arguments }
}

function Find-Element([string]$Using, [string]$Value) {
    $element = Session-WD POST '/element' @{ using=$Using; value=$Value }
    return $element.'element-6066-11e4-a52e-4f735466cecf'
}

function Click-Button([string]$Text) {
    $scope = ''
    if (Execute-JS 'return !!document.querySelector("[role=dialog]");') { $scope='//*[@role="dialog"]' }
    $element = Find-Element xpath "$scope//button[normalize-space(.)='$Text']"
    [void](Session-WD POST "/element/$element/click" @{})
}

function Fill-Element([string]$Using, [string]$Selector, [string]$Text) {
    $element = Find-Element $Using $Selector
    [void](Session-WD POST "/element/$element/clear" @{})
    [void](Session-WD POST "/element/$element/value" @{ text=$Text })
}

function Send-TerminalKeys([string]$Keys) {
    # Never clear or replace xterm's input value: editing must reach the PTY.
    $element = Find-Element 'css selector' '.session-view textarea.xterm-helper-textarea'
    [void](Session-WD POST "/element/$element/value" @{ text=$Keys })
}

function Send-Terminal([string]$Text) {
    Send-TerminalKeys ($Text + [char]0xE007)
}

function Claim-Terminal {
    Click-Selector '.session-view .terminal-canvas'
    [void](Wait-Until { Body-Contains 'Controlling' } 'terminal click acquires available writer lease')
}

function Test-DirectTerminalKeys([string]$Session) {
    Assert-True ($Session -cmatch '^[0-9a-f]{32}$') 'Unexpected disposable session ID'
    # Only the disposable fixture shell is changed. Its Bash reads no user
    # configuration, writes no shell history, and keeps the test PTY intact.
    Send-Terminal 'exec env HISTFILE=/dev/null INPUTRC=/dev/null bash --noprofile --norc -i'
    Send-Terminal "printf 'RELAY_%s\n' 'DIRECT_READY'"
    [void](Wait-Until { Terminal-Contains 'RELAY_DIRECT_READY' } 'isolated readline shell')
    Send-TerminalKeys ("printf 'RELAY_%s\n' 'KEYS_OKX'" + [char]0xE012 + [char]0xE003 + [char]0xE014 + [char]0xE007)
    [void](Wait-Until { Terminal-Contains 'RELAY_KEYS_OK' } 'direct Left/Backspace/Right editing')
    Assert-True (-not (Terminal-Contains 'RELAY_KEYS_OKX')) 'Backspace did not edit the remote command'
    $name = 'relay_tab_probe_' + $Session + '_complete'
    Send-Terminal ("printf 'RELAY_%s\n' 'TAB_OK' > " + $name)
    Send-TerminalKeys ('cat ' + $name.Substring(0,$name.Length-4) + [char]0xE004 + [char]0xE007)
    [void](Wait-Until { Terminal-Contains 'RELAY_TAB_OK' } 'direct Tab filename completion')
    Send-Terminal ('rm -- ' + $name)
    Send-Terminal "printf 'RELAY_%s\n' 'INTERRUPT_READY'; sleep 30"
    [void](Wait-Until { Terminal-Contains 'RELAY_INTERRUPT_READY' } 'foreground command ready for Ctrl+C')
    Send-TerminalKeys ([string][char]0xE009 + 'c' + [char]0xE000)
    Send-Terminal "printf 'RELAY_%s\n' 'INTERRUPTED'"
    [void](Wait-Until { Terminal-Contains 'RELAY_INTERRUPTED' } 'Ctrl+C interrupts foreground command' 5)
    Write-Host 'PASS: direct terminal arrows, Backspace, Tab and Ctrl+C'
}

function Test-TerminalClipboard {
    $command = "printf 'RELAY_%s\n' 'NATIVE_CLIPBOARD_OK'"
    $copied = Execute-AsyncJS 'const done=arguments[arguments.length-1];navigator.clipboard.writeText(arguments[0]).then(()=>done(true),()=>done(false));' @($command)
    Assert-True ($copied -eq $true) 'Native clipboard text write was denied'
    Send-TerminalKeys ([string][char]0xE009 + 'v' + [char]0xE000)
    [void](Wait-Until { Terminal-Contains $command } 'Ctrl+V pastes native clipboard text')
    Assert-True (-not (Terminal-Contains 'RELAY_NATIVE_CLIPBOARD_OK')) 'Pasting text unexpectedly submitted the command'
    Send-TerminalKeys ([string][char]0xE007)
    [void](Wait-Until { Terminal-Contains 'RELAY_NATIVE_CLIPBOARD_OK' } 'pasted command executes only after Enter')
    Send-Terminal "printf '\033[2J\033[HRELAY_COPY_ME\n'"
    [void](Wait-Until { Execute-JS 'return document.querySelector(".xterm-rows > div")?.textContent.trim()==="RELAY_COPY_ME";' } 'terminal copy fixture')
    $point = Execute-JS 'const r=document.querySelector(".xterm-rows > div").getBoundingClientRect();return {x:Math.round(r.x+15),y:Math.round(r.y+r.height/2)};'
    [void](Session-WD POST '/actions' @{ actions=@(@{
        type='pointer'; id='clipboard-mouse'; parameters=@{pointerType='mouse'}; actions=@(
            @{type='pointerMove';duration=0;origin='viewport';x=$point.x;y=$point.y},
            @{type='pointerDown';button=0}, @{type='pointerUp';button=0},
            @{type='pause';duration=80},
            @{type='pointerDown';button=0}, @{type='pointerUp';button=0}
        )
    }) })
    [void](Wait-Until { Execute-JS 'return document.querySelector("[aria-label=\"Copy terminal selection\"]")?.disabled===false;' } 'terminal text selection')
    Send-TerminalKeys ([string][char]0xE009 + 'c' + [char]0xE000)
    $text = Execute-AsyncJS 'const done=arguments[arguments.length-1];navigator.clipboard.readText().then(done,()=>done(null));'
    Assert-True ($text -ceq 'RELAY_COPY_ME') 'Ctrl+C did not copy the selected native terminal text'
    Write-Host 'PASS: native Windows text clipboard, Ctrl+V paste and Ctrl+C selection copy'
}

function Send-Setup([string]$Text) {
    # Setup deliberately has no composer: exercise xterm's real keyboard input.
    $element = Find-Element 'css selector' '.setup-terminal textarea.xterm-helper-textarea'
    [void](Session-WD POST "/element/$element/value" @{ text=($Text + [char]0xE007) })
}

function Body-Contains([string]$Text) {
    return Execute-JS 'return document.body && document.body.innerText.includes(arguments[0]);' @($Text)
}

function Terminal-Contains([string]$Text) {
    return Execute-JS 'const n=document.querySelector(".xterm-rows"); return !!n && n.textContent.includes(arguments[0]);' @($Text)
}

function Save-Screenshot([string]$Name) {
    $data = Session-WD GET '/screenshot'
    [IO.File]::WriteAllBytes((Join-Path $script:Artifacts $Name), [Convert]::FromBase64String($data))
}

function API([string]$Path, [string]$Method = 'GET', $Body = $null) {
    return HTTP $script:BrowserHTTP $Method ($script:Origin + $Path) $Body @{ Origin=$script:Origin; 'X-Relay-CSRF'=$script:CSRF }
}

function API-JSON([string]$Path) {
    $response = API $Path
    Assert-True ($response.Status -eq 200) "Second browser API failed: $($response.Status)"
    return $response.Body | ConvertFrom-Json
}

function Fresh-Launch {
    $helper = Start-OwnedProcess $script:Controller @('desktop', '--state-dir', $script:StateDirectory,
        '--runtime-dir', $script:RuntimeDirectory, '--local=false', '--binaries', $script:Bundle) $script:TestEnvironment
    try {
        Assert-True ($helper.Process.WaitForExit(30000)) 'Native controller helper timed out'
        Assert-True ($helper.Process.ExitCode -eq 0) 'Native controller helper failed; login output withheld'
        $output = $helper.Output.GetAwaiter().GetResult()
        Assert-True ($output.Length -lt 16384) 'Oversized native controller handshake'
        $launch = $output | ConvertFrom-Json
        $address = [Uri]$launch.address
        $login = [Uri]$launch.url
        Assert-True ($launch.protocol -eq 1 -and $address.Scheme -eq 'http' -and
            $address.Host -eq '127.0.0.1' -and $address.Port -gt 0 -and $login.Authority -eq $address.Authority -and
            $login.Scheme -eq 'http' -and $login.AbsolutePath -eq '/auth' -and -not $login.UserInfo -and -not $login.Fragment) 'Invalid native launch destination'
        return $launch
    } finally { Stop-OwnedProcess $helper }
}

function App-Identity {
    $processes = @(Get-CimInstance Win32_Process | Select-Object ProcessId, ParentProcessId, Name, ExecutablePath)
    $descendants = @($script:DriverProcess.Process.Id)
    for ($round=0; $round -lt 6; $round++) {
        $children = @($processes | Where-Object { $_.ParentProcessId -in $descendants -and $_.ProcessId -notin $descendants })
        if ($children.Count -eq 0) { break }
        $descendants += @($children | ForEach-Object { [int]$_.ProcessId })
    }
    Assert-True (-not ($processes | Where-Object { $_.ProcessId -in $descendants -and $_.Name -eq 'wsl.exe' })) 'Native app unexpectedly launched WSL'
    $matches = @($processes | Where-Object { $_.ProcessId -in $descendants -and
        [string]::Equals($_.ExecutablePath, $script:Desktop, [StringComparison]::OrdinalIgnoreCase) })
    # Keep facts from this exact identity check for failure evidence. Do not
    # include general processes, window titles, command lines or login URLs.
    $script:LastNativeLaunchDiagnostic = @{
        sampledAt=[DateTimeOffset]::UtcNow.ToString('o'); driverPid=$script:DriverProcess.Process.Id;
        expectedPath=$script:Desktop; matches=$matches;
        ownedProcesses=@($processes | Where-Object { $_.ProcessId -in $descendants }); windows=@{}
    }
    foreach ($item in $matches) {
        $script:LastNativeLaunchDiagnostic.windows[[string]$item.ProcessId] = @([RelayWindowProbe]::OwnedWindows($item.ProcessId))
    }
    if ($matches.Count -ne 1) { return $null }
    $identity = Process-Identity $matches[0].ProcessId $script:Desktop
    $process = Get-Process -Id $identity.Id
    $process.Refresh()
    $window = [RelayWindowProbe]::MainFrame($identity.Id)
    if ($window -eq [IntPtr]::Zero) { return $null }
    $identity | Add-Member NoteProperty Window $window
    return $identity
}

function Save-NativeWindow($Identity, [string]$Name = 'native-window.png', [switch]$CheckContent) {
    $rectangle = New-Object RelayWindowProbe+Rect
    [uint32]$owner = 0
    [void][RelayWindowProbe]::GetWindowThreadProcessId($Identity.Window, [ref]$owner)
    Assert-True ($owner -eq $Identity.Id) 'Screenshot window does not belong to native Relay'
    Assert-True ([RelayWindowProbe]::GetWindowRect($Identity.Window, [ref]$rectangle)) 'Could not inspect native window'
    $width, $height = ($rectangle.Right-$rectangle.Left), ($rectangle.Bottom-$rectangle.Top)
    Assert-True ($width -gt 100 -and $height -gt 100 -and $width -le 4096 -and $height -le 4096) 'Unexpected native window dimensions'
    $bitmap = New-Object Drawing.Bitmap($width, $height)
    $graphics = [Drawing.Graphics]::FromImage($bitmap)
    try {
        $dc = $graphics.GetHdc()
        try { $captured = [RelayWindowProbe]::PrintWindow($Identity.Window, $dc, 2) }
        finally { $graphics.ReleaseHdc($dc) }
        Assert-True $captured 'Native window capture failed'
        $bitmap.Save((Join-Path $script:Artifacts $Name), [Drawing.Imaging.ImageFormat]::Png)
        if ($CheckContent) {
            # Require contrasting content below the native chrome, rather than
            # accepting an entirely blank light or dark background. This only
            # captures the exact owned HWND, never the screen or clipboard.
            $lightPixels = $darkPixels = 0
            for ($y = 90; $y -lt $height - 20; $y += 8) {
                for ($x = [int]($width / 4); $x -lt $width - 20; $x += 8) {
                    $pixel = $bitmap.GetPixel($x, $y)
                    $luminance = 0.2126 * $pixel.R + 0.7152 * $pixel.G + 0.0722 * $pixel.B
                    if ($luminance -gt 180) {
                        $lightPixels++
                    } elseif ($luminance -lt 120) { $darkPixels++ }
                    if ($lightPixels -ge 40 -and $darkPixels -ge 40) { return $true }
                }
            }
            return $false
        }
    } finally { $graphics.Dispose(); $bitmap.Dispose() }
}

function Select-NativeView([string]$Kind) {
    foreach ($handle in @(Session-WD GET '/window/handles')) {
        try {
            [void](Session-WD POST '/window' @{ handle=$handle })
            $url = [Uri](Session-WD GET '/url')
            $matches = switch ($Kind) {
                'content' { $url.Scheme -eq 'http' -and $url.Host -eq '127.0.0.1' }
                'chrome' { $url.Host -eq 'tauri.localhost' -and $url.AbsolutePath -eq '/chrome.html' }
                'menu' { $url.Host -eq 'tauri.localhost' -and $url.AbsolutePath -eq '/menu.html' }
                default { throw 'Unknown native webview role' }
            }
            if ($matches) {
                if ($Kind -eq 'chrome' -or $Kind -eq 'menu') {
                    [void](Execute-JS @'
if (!window.relayFrameTrace) {
  window.relayFrameTrace = [];
  const record = (type, detail) => {
    const log = window.relayFrameTrace;
    log.push({at:Date.now(), type, focused:document.hasFocus(), expanded:document.getElementById('app-menu-toggle')?.getAttribute('aria-expanded'), detail});
    if(log.length>160) log.shift();
  };
  ['focus','blur','pointerdown','pointerup','mousedown','mouseup','click','relay-menu-open','relay-menu-check-focus','relay-chrome-check-focus','relay-menu-confirm-blur'].forEach(type => window.addEventListener(type, event => record(type, {generation:event.detail?.generation, target:event.target?.id, trusted:event.isTrusted, x:event.clientX,y:event.clientY}), true));
  const toggle=document.getElementById('app-menu-toggle');
  if(toggle) new MutationObserver(() => record('expanded', null)).observe(toggle,{attributes:true,attributeFilter:['aria-expanded']});
}

'@)
                }
                return $true
            }
        } catch { }
    }
    return $false
}

function Wait-NativeGeometry([string]$Label) {
    return Wait-Until {
        $geometry = [RelayWindowProbe]::FrameGeometry($script:NativeIdentity.Window)
        if (-not $geometry -or $geometry.Views.Length -ne 3 -or $geometry.DPI -eq 0) { return $false }
        $scale = $geometry.DPI / 96.0
        $header = [int][Math]::Round(40 * $scale, [MidpointRounding]::AwayFromZero)
        $main = @($geometry.Views | Where-Object { $_.Left -eq 0 -and $_.Top -eq $header -and $_.Right -eq $geometry.Width -and $_.Bottom -eq $geometry.Height })
        $chrome = @($geometry.Views | Where-Object { $_.Left -eq 0 -and $_.Top -eq 0 -and $_.Right -eq $geometry.Width -and $_.Bottom -eq $header })
        if ($main.Count -ne 1 -or $chrome.Count -ne 1) { return $false }
        if (-not (Select-NativeView 'chrome')) { return $false }
        $chromeDOM = Execute-JS 'return {width:innerWidth,height:innerHeight,dpr:devicePixelRatio};'
        if (-not (Select-NativeView 'content')) { return $false }
        $contentDOM = Execute-JS 'return {width:innerWidth,height:innerHeight,dpr:devicePixelRatio};'
        # innerWidth/Height are integral CSS pixels; native HWND rectangles
        # above require exact physical coverage, while DOM rounding allows one
        # CSS pixel at the current (unchanged) monitor DPI.
        foreach ($view in @(@{dom=$chromeDOM; height=$header}, @{dom=$contentDOM; height=$geometry.Height-$header})) {
            if ([Math]::Abs($view.dom.dpr - $scale) -gt 0.01 -or
                [Math]::Abs($view.dom.width * $scale - $geometry.Width) -gt $scale + 0.1 -or
                [Math]::Abs($view.dom.height * $scale - $view.height) -gt $scale + 0.1) { return $false }
        }
        return [pscustomobject]@{ label=$Label; width=$geometry.Width; height=$geometry.Height; dpi=$geometry.DPI; header=$header; content=$contentDOM; chrome=$chromeDOM }
    } "exact native frame geometry ($Label)"
}

function Test-NativeResizeGeometry {
    $window = $script:NativeIdentity.Window
    $original = New-Object RelayWindowProbe+Rect
    Assert-True ([RelayWindowProbe]::GetWindowRect($window, [ref]$original)) 'Could not read owned window size'
    $records = @()
    try {
        foreach ($size in @(@(1003,733), @(1291,863), @(841,641))) {
            Assert-True ([RelayWindowProbe]::SetWindowPos($window,[IntPtr]::Zero,0,0,$size[0],$size[1],0x0016)) 'Could not resize owned frame'
            $records += Wait-NativeGeometry ("resize-{0}x{1}" -f $size[0],$size[1])
        }
        [void](Wait-Until { Select-NativeView 'chrome' } 'trusted chrome before maximize')
        Assert-True (Execute-AsyncJS 'const done=arguments[arguments.length-1];window.__TAURI_INTERNALS__.invoke("frame_action",{action:"maximize"}).then(()=>done(true),()=>done(false));') 'Trusted maximize failed'
        [void](Wait-Until { [RelayWindowProbe]::IsZoomed($window) } 'owned frame maximized')
        $records += Wait-NativeGeometry 'maximized'
        [void](Wait-Until { Select-NativeView 'chrome' } 'trusted chrome before restore')
        Assert-True (Execute-AsyncJS 'const done=arguments[arguments.length-1];window.__TAURI_INTERNALS__.invoke("frame_action",{action:"maximize"}).then(()=>done(true),()=>done(false));') 'Trusted restore failed'
        [void](Wait-Until { -not [RelayWindowProbe]::IsZoomed($window) } 'owned frame restored')
        $records += Wait-NativeGeometry 'restored'
    } finally {
        if ([RelayWindowProbe]::IsZoomed($window)) { [void][RelayWindowProbe]::ShowWindow($window, 9) }
        [void][RelayWindowProbe]::SetWindowPos($window,[IntPtr]::Zero,$original.Left,$original.Top,$original.Right-$original.Left,$original.Bottom-$original.Top,0x0014)
        [void](Wait-NativeGeometry 'original')
    }
    $records | ConvertTo-Json -Depth 6 | Set-Content -LiteralPath (Join-Path $script:Artifacts 'native-frame-geometry.json') -Encoding UTF8
    Write-Host 'PASS: exact native content/title-bar physical coverage across odd sizes, maximize and restore at the current monitor DPI'
}

function Test-TerminalResizeRendering {
    $window = $script:NativeIdentity.Window
    $original = New-Object RelayWindowProbe+Rect
    Assert-True ([RelayWindowProbe]::GetWindowRect($window, [ref]$original)) 'Could not read terminal window size'
    $records = @()
    try {
        foreach ($case in @(@{label='1003x733'; width=1003; height=733}, @{label='1291x863'; width=1291; height=863}, @{label='841x641'; width=841; height=641}, @{label='maximized'}, @{label='restored'}, @{label='larger-text'; font='increase'}, @{label='default-text'; font='reset'})) {
            if ($case.ContainsKey('width')) {
                Assert-True ([RelayWindowProbe]::SetWindowPos($window,[IntPtr]::Zero,0,0,$case.width,$case.height,0x0016)) 'Could not resize terminal frame'
            } elseif ($case.ContainsKey('font')) {
                if ($case.font -eq 'increase') {
                    1..3 | ForEach-Object { Click-Selector '[aria-label="Increase terminal font size"]' }
                } else { Click-Selector '[aria-label="Reset terminal font size"]' }
            } else {
                [void](Wait-Until { Select-NativeView 'chrome' } 'trusted frame for terminal maximize/restore')
                Assert-True (Execute-AsyncJS 'const done=arguments[arguments.length-1];window.__TAURI_INTERNALS__.invoke("frame_action",{action:"maximize"}).then(()=>done(true),()=>done(false));') 'Could not maximize/restore terminal frame'
                [void](Wait-Until { [RelayWindowProbe]::IsZoomed($window) -eq ($case.label -eq 'maximized') } 'terminal maximize/restore')
            }
            [void](Wait-NativeGeometry ("terminal-" + $case.label))
            $geometry = Wait-Until {
                Execute-JS @'
const shell=document.querySelector('.session-view .terminal-shell');
const mount=shell?.querySelector('.terminal-mount'), screen=shell?.querySelector('.xterm-screen');
if(!shell||!mount||!screen)return false;
const cols=Number(shell.dataset.terminalCols),rows=Number(shell.dataset.terminalRows);
if(!cols||!rows)return false;
const m=mount.getBoundingClientRect(),s=screen.getBoundingClientRect();
if(!s.width||!s.height||s.left<m.left-1||s.top<m.top-1||s.right>m.right+1||s.bottom>m.bottom+1)return false;
const scrollbar=shell.querySelector(".xterm-scrollable-element > .scrollbar.vertical")?.getBoundingClientRect().width||0;
if(m.width-s.width>scrollbar+s.width/cols+2||m.height-s.height>s.height/rows+2)return false;
if(!shell.querySelector('[aria-label="Terminal dimensions"]')?.textContent.includes(`${cols} \u00d7 ${rows}`))return false;
return {cols,rows,dpr:devicePixelRatio,mount:{width:m.width,height:m.height},screen:{width:s.width,height:s.height},rowElements:shell.querySelectorAll('.xterm-rows>div').length};
'@
            } 'terminal fits its unpadded viewport and reports current dimensions'
            $marker = 'RELAY_SIZE_' + [Guid]::NewGuid().ToString('N').Substring(0,8)
            Send-Terminal ("printf '{0} '; stty size" -f $marker)
            $expected = "$marker $($geometry.rows) $($geometry.cols)"
            [void](Wait-Until { Terminal-Contains $expected } 'remote PTY geometry equals the displayed terminal dimensions')
            $geometry | Add-Member NoteProperty label $case.label
            $records += $geometry
            Save-NativeWindow $script:NativeIdentity ("native-terminal-" + $case.label + '.png')
        }
    } finally {
        if ([RelayWindowProbe]::IsZoomed($window)) { [void][RelayWindowProbe]::ShowWindow($window, 9) }
        [void][RelayWindowProbe]::SetWindowPos($window,[IntPtr]::Zero,$original.Left,$original.Top,$original.Right-$original.Left,$original.Bottom-$original.Top,0x0014)
        [void](Wait-NativeGeometry 'terminal-original')
    }
    $records | ConvertTo-Json -Depth 6 | Set-Content -LiteralPath (Join-Path $script:Artifacts 'native-terminal-geometry.json') -Encoding UTF8
    Write-Host 'PASS: native terminal screen fits the viewport and real remote stty dimensions match after every owned-window resize'
}

function Click-Selector([string]$Selector) {
    $element = Find-Element 'css selector' $Selector
    [void](Session-WD POST "/element/$element/click" @{})
}

function Click-NativeSelector([string]$Selector, [int]$ContentOffset = 0) {
    # WebDriver's synthetic click does not reliably transfer OS focus between
    # sibling WebView2 controls. Send a real click only inside our owned HWND.
    $point = Execute-JS 'const r=document.querySelector(arguments[0]).getBoundingClientRect(); return {x:(r.x+r.width/2)*devicePixelRatio,y:(r.y+r.height/2+arguments[1])*devicePixelRatio};' @($Selector,$ContentOffset)
    $window = $script:NativeIdentity.Window
    $oldDpi = [RelayWindowProbe]::SetThreadDpiAwarenessContext([IntPtr](-4))
    try {
    [void][RelayWindowProbe]::SetForegroundWindow($window)
    # Windows may refuse programmatic foreground activation. The frame test
    # holds only its own window above others through the focus checks, then
    # restores stacking in finally. Dropping TOPMOST mid-check can itself
    # dismiss the popup. Never click through another window.
    $positioned = [RelayWindowProbe]::SetWindowPos($window,[IntPtr](-1),0,0,0,0,0x0013)
    Start-Sleep -Milliseconds 100
    $origin = New-Object RelayWindowProbe+Point
    [void][RelayWindowProbe]::ClientToScreen($window,[ref]$origin)
    $target = New-Object RelayWindowProbe+Point
    $target.X = $origin.X + [int]$point.x; $target.Y = $origin.Y + [int]$point.y
    $hitWindow = [RelayWindowProbe]::WindowFromPoint($target)
    $hitRoot = [RelayWindowProbe]::GetAncestor($hitWindow,2)
    $rect = New-Object RelayWindowProbe+Rect
    [void][RelayWindowProbe]::GetWindowRect($window,[ref]$rect)
    [int]$cloaked = 0
    [void][RelayWindowProbe]::DwmGetWindowAttribute($window,14,[ref]$cloaked,4)
    $extendedStyle = [RelayWindowProbe]::GetWindowLong($window,-20)
    $script:LastNativeClick = @{ extendedStyle=$extendedStyle; noActivate=($extendedStyle -band 0x08000000) -ne 0; topmost=($extendedStyle -band 8) -ne 0; selector=$Selector; point=@($target.X,$target.Y); expected=[long]$window; hit=[long]$hitRoot; foreground=[long][RelayWindowProbe]::GetForegroundWindow(); positioned=$positioned; cloaked=$cloaked; at=[DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds() }
    $script:NativeClicks = @($script:NativeClicks | Where-Object { $null -ne $_ }) + $script:LastNativeClick
    Assert-True ($hitRoot -eq $window) "Native click target is outside the owned Relay window (selector=$Selector, point=$($target.X),$($target.Y), expected=$window, actual=$hitRoot, origin=$($origin.X),$($origin.Y), rect=$($rect.Left),$($rect.Top),$($rect.Right),$($rect.Bottom), positioned=$positioned, cloaked=$cloaked)"
    $previous = New-Object RelayWindowProbe+Point
    [void][RelayWindowProbe]::GetCursorPos([ref]$previous)
    try {
        # One SendInput batch keeps move/down/up contiguous instead of mixing
        # our separate mouse_event calls with another physical input stream.
        Assert-True ([RelayWindowProbe]::ClickAt($target.X,$target.Y)) 'Windows rejected native pointer input'
        Start-Sleep -Milliseconds 100
        $script:LastNativeClick.afterForeground = [long][RelayWindowProbe]::GetForegroundWindow()
        Assert-True ($script:LastNativeClick.afterForeground -eq [long]$window) 'Windows did not activate the owned test frame after physical input; no menu behavior is inferred from this focus failure'
    } finally { [void][RelayWindowProbe]::SetCursorPos($previous.X,$previous.Y) }
    } finally {
        if (-not $script:FrameHoldsTopmost) { [void][RelayWindowProbe]::SetWindowPos($window,[IntPtr](-2),0,0,0,0,0x0013) }
        if ($oldDpi -ne [IntPtr]::Zero) { [void][RelayWindowProbe]::SetThreadDpiAwarenessContext($oldDpi) }
    }
}

function Save-FrameTrace {
    if (-not $script:SessionID) { return }
    $current = Session-WD GET '/window'
    try {
        $diagnostic = @{ nativeClicks=@($script:NativeClicks); foreground=[long][RelayWindowProbe]::GetForegroundWindow(); views=@{} }
        foreach ($kind in @('chrome','menu')) {
            if (Select-NativeView $kind) { $diagnostic.views[$kind] = Execute-JS 'return window.relayFrameTrace || [];' }
        }
        $diagnostic | ConvertTo-Json -Depth 12 | Set-Content -Encoding UTF8 -LiteralPath (Join-Path $script:Artifacts 'native-frame-events.json')
    } finally { [void](Session-WD POST '/window' @{handle=$current}) }
}

function Test-CustomFrame {
    $window = $script:NativeIdentity.Window
    $wasTopmost = ([RelayWindowProbe]::GetWindowLong($window,-20) -band 8) -ne 0
    $script:FrameHoldsTopmost = $true
    try {
        Assert-True ([RelayWindowProbe]::SetWindowPos($window,[IntPtr](-1),0,0,0,0,0x0013)) 'Could not raise the owned frame for physical input'
        [void](Wait-Until { Select-NativeView 'menu' } 'initialize menu diagnostics')
        Assert-True (Execute-JS 'return JSON.stringify([...document.querySelectorAll("[data-action]")].map(button=>button.dataset.action)) === JSON.stringify(["reload","reconnect","close"]);') 'Desktop menu contains an unexpected browser action'
        Test-CustomFrameBody
    } finally {
        $script:FrameHoldsTopmost = $false
        if (-not $wasTopmost) { [void][RelayWindowProbe]::SetWindowPos($window,[IntPtr](-2),0,0,0,0,0x0013) }
    }
}

function Test-CustomFrameBody {
    $window = $script:NativeIdentity.Window
    $outer = New-Object RelayWindowProbe+Rect
    $client = New-Object RelayWindowProbe+Point
    [void][RelayWindowProbe]::GetWindowRect($window,[ref]$outer)
    [void][RelayWindowProbe]::ClientToScreen($window,[ref]$client)
    Assert-True (($client.Y-$outer.Top) -le 12) 'Native title bar still occupies the top of the window'
    [void](Wait-Until { Select-NativeView 'chrome' } 'trusted custom title bar')
    Assert-True (Execute-JS 'return !!document.querySelector("#window-drag-region");') 'Custom drag region is missing'
    Click-Selector '[aria-label="Maximize window"]'
    [void](Wait-Until { [RelayWindowProbe]::IsZoomed($window) } 'custom maximize control')
    [void](Wait-Until { Execute-JS 'return !!document.querySelector("[aria-label=\"Restore window\"]");' } 'custom restore control state')
    Click-Selector '[aria-label="Restore window"]'
    [void](Wait-Until { -not [RelayWindowProbe]::IsZoomed($window) } 'custom restore control')
    Click-Selector '[aria-label="Minimize window"]'
    [void](Wait-Until { [RelayWindowProbe]::IsIconic($window) } 'custom minimize control')
    [void][RelayWindowProbe]::ShowWindow($window, 9)
    [void][RelayWindowProbe]::SetForegroundWindow($window)
    [void](Wait-Until { -not [RelayWindowProbe]::IsIconic($window) } 'restore the exact owned test window')

    $before = New-Object RelayWindowProbe+Rect
    [void][RelayWindowProbe]::GetWindowRect($window, [ref]$before)
    # WebDriver pointer events are synthetic WebView events and do not drive
    # Windows' OS-level mouse capture/move loop. Check layout using the exact
    # owned HWND; the title bar delegates actual dragging to Tauri.
    [void][RelayWindowProbe]::SetWindowPos($window,[IntPtr]::Zero,100,100,1140,790,0x0014)
    [void](Wait-Until { Select-NativeView 'content' } 'content after native resize')
    $size = Execute-JS 'return {width:innerWidth,height:innerHeight};'
    Assert-True ($size.width -ge 800 -and $size.height -ge 450) 'Resizing lost the controller content area'
    $denied = Session-WD POST '/execute/async' @{ script='const done=arguments[arguments.length-1]; const invoke=window.__TAURI_INTERNALS__?.invoke; if (!invoke) { done(true); } else { invoke("frame_action",{action:"maximize"}).then(()=>done(false),()=>done(true)); }'; args=@() }
    Assert-True $denied 'Controller content invoked a trusted native frame action'
    Assert-True (-not [RelayWindowProbe]::IsZoomed($window)) 'Untrusted content changed native window state'
    [void](Wait-Until { Select-NativeView 'chrome' } 'chrome before native application menu')
    Click-NativeSelector '[aria-label="Relay menu"]'
    # Remain in the title-bar view: selecting the menu's WebDriver target can
    # refocus an already-dismissed popup and conceal an opening focus race.
    Start-Sleep -Milliseconds 500
    Assert-True (Execute-JS 'return document.querySelector("[aria-label=\"Relay menu\"]").getAttribute("aria-expanded") === "true";') 'Relay menu dismissed itself immediately after opening'
    [void](Wait-Until { (Select-NativeView 'menu') -and (Body-Contains 'Reconnect') } 'custom Relay menu')
    # A transient child-view blur must not close a menu that still owns focus.
    Assert-True (Execute-JS 'document.querySelector("button").focus(); window.dispatchEvent(new Event("blur")); return document.hasFocus();') 'Menu did not receive keyboard focus'
    Start-Sleep -Milliseconds 500
    Assert-True (Execute-JS 'return document.hasFocus();') 'Transient child-view blur dismissed the focused menu'
    $menuButton = Find-Element 'css selector' 'button'
    [void](Session-WD POST "/element/$menuButton/value" @{ text=([string][char]0xE00C) })
    [void](Wait-Until { Select-NativeView 'chrome' } 'title bar after menu Escape')
    Assert-True (Execute-JS 'return document.querySelector("[aria-label=\"Relay menu\"]").getAttribute("aria-expanded") === "false" && document.activeElement.id === "app-menu-toggle";') 'Escape did not close the menu and return focus to its trigger'
    Click-NativeSelector '[aria-label="Relay menu"]'
    Start-Sleep -Milliseconds 300
    Click-NativeSelector '[aria-label="Relay menu"]'
    Start-Sleep -Milliseconds 300
    Assert-True (Execute-JS 'return document.querySelector("[aria-label=\"Relay menu\"]").getAttribute("aria-expanded") === "false";') 'A second trigger click did not toggle the menu closed'
    Click-NativeSelector '[aria-label="Relay menu"]'
    [void](Wait-Until { Select-NativeView 'content' } 'content outside the open menu')
    # The sidebar search is underneath the popup; click a visible content
    # control to its right, as a person would when dismissing the menu.
    Click-NativeSelector '.topbar-breadcrumb button' 40
    Start-Sleep -Milliseconds 500
    [void](Wait-Until { Select-NativeView 'chrome' } 'title bar after outside click')
    Assert-True (Execute-JS 'return document.querySelector("[aria-label=\"Relay menu\"]").getAttribute("aria-expanded") === "false";') 'Outside focus did not dismiss the menu'
    Click-NativeSelector '[aria-label="Relay menu"]'
    [void](Wait-Until { Select-NativeView 'menu' } 'menu reopened for reload')
    Save-NativeWindow $script:NativeIdentity 'native-custom-menu.png'
    Click-Selector '[data-action="reload"]'
    [void](Wait-Until { (Select-NativeView 'content') -and (Execute-JS 'return document.querySelector(".page-heading h1")?.textContent === "Fleet overview";') } 'custom menu reload')
    Save-FrameTrace
    Write-Host 'PASS: custom title bar, minimize/maximize/restore, native resize, menu reload and privileged-action isolation'
}

function Close-NativeWindow {
    [void](Wait-Until { Select-NativeView 'chrome' } 'chrome close control')
    Click-Selector '[aria-label="Close window"]'
    [void](Wait-Until { $null -eq (Get-Process -Id $script:NativeIdentity.Id -ErrorAction SilentlyContinue) } 'custom close exits the owned native app')
}

function Start-AppSession {
    $result = WD POST '/session' @{ capabilities=@{ alwaysMatch=@{
        browserName='webview2'; 'ms:edgeChromium'=$true; 'ms:edgeOptions'=@{
            binary=$script:Desktop;
            # EdgeDriver otherwise enables browser logging, which can allocate
            # a visible DevTools console in WebView2. These are test-only flags.
            excludeSwitches=@('enable-logging');
            webviewOptions=@{
                userDataFolder=$script:TestEnvironment.WEBVIEW2_USER_DATA_FOLDER;
                additionalBrowserArguments=@('--disable-logging', '--log-level=3')
            }
        }
    } } }
    $script:SessionID = $result.sessionId
    Assert-True ($result.capabilities.browserName -eq 'webview2') 'Driver did not start native WebView2'
    [void](Session-WD POST '/timeouts' @{ implicit=0; pageLoad=30000; script=10000 })
    [void](Wait-Until { (Select-NativeView 'content') -and (Execute-JS 'return document.querySelector(".page-heading h1")?.textContent === "Fleet overview";') } 'native Windows fleet rendering' 90)
    $script:NativeIdentity = Wait-Until { App-Identity } 'native PE process and Win32 window'
}

function Close-AppSession {
    if ($script:SessionID) {
        try { [void](Session-WD DELETE '') } catch { }
        $script:SessionID = $null
    }
}

function Read-NativeAppearance {
    $result = Execute-AsyncJS @'
const done=arguments[arguments.length-1];
if (!window.__TAURI_INTERNALS__?.invoke) { done({ok:false}); return; }
window.__TAURI_INTERNALS__.invoke('appearance',{}).then(value=>done({ok:true,value}),()=>done({ok:false}));
'@
    Assert-True $result.ok 'Native appearance IPC is unavailable'
    $state = $result.value
    Assert-True ($state.preference -cin @('system','light','dark') -and $state.resolved -cin @('light','dark')) 'Native appearance returned an invalid state'
    return $state
}

function Assert-AppearanceViews($State) {
    try {
        foreach ($kind in @('content','chrome','menu')) {
            [void](Wait-Until { Select-NativeView $kind } "appearance target $kind")
            [void](Wait-Until {
                Execute-JS 'return document.documentElement.dataset.theme === arguments[0] && getComputedStyle(document.documentElement).colorScheme === arguments[0];' @($State.resolved)
            } "shared $($State.resolved) appearance in $kind")
            $actual = Read-NativeAppearance
            Assert-True ($actual.preference -ceq $State.preference -and $actual.resolved -ceq $State.resolved) "Native $kind disagrees on the shared preference"
            $luminance = Execute-JS @'
const color=getComputedStyle(document.body).backgroundColor.match(/^rgba?\(([^)]+)\)$/);
if(!color) return null;
const channels=color[1].split(',').map(Number);
if(channels.length>3 && channels[3]!==1) return null;
return channels.slice(0,3).reduce((total,channel)=>total+channel,0)/3;
'@
            Assert-True ($null -ne $luminance) "Native $kind has no opaque themed surface"
            Assert-True (($State.resolved -ceq 'light' -and $luminance -gt 200) -or ($State.resolved -ceq 'dark' -and $luminance -lt 70)) "Native $kind retained the wrong palette"
        }
    } finally {
        [void](Wait-Until { Select-NativeView 'content' } 'restore controller after appearance checks')
    }
}

function Select-AppAppearance([string]$Preference) {
    Assert-True ($Preference -cin @('system','light','dark')) 'Invalid test appearance preference'
    [void](Wait-Until { Select-NativeView 'content' } 'controller appearance control')
    [void](Wait-Until { Execute-JS 'const select=document.querySelector("select[aria-label=Appearance]");return !!select && !select.disabled;' } 'appearance control ready')
    # Dispatch the select's normal React change handler. No physical input or
    # operating-system preference is changed; the app performs the native IPC.
    Assert-True (Execute-JS 'const select=document.querySelector("select[aria-label=Appearance]");if(!select || select.disabled)return false;select.value=arguments[0];select.dispatchEvent(new Event("change",{bubbles:true}));return true;' @($Preference)) 'Could not choose the app appearance'
    [void](Wait-Until { Execute-JS 'const select=document.querySelector("select[aria-label=Appearance]");return !!select && !select.disabled && select.value===arguments[0] && !document.querySelector(".appearance-notice");' @($Preference) } 'appearance UI confirms native save')
    $state = Read-NativeAppearance
    Assert-True ($state.preference -ceq $Preference) 'UI choice did not reach the native preference store'
    if ($Preference -ceq 'system') {
        $expected = Execute-JS 'return matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";'
        Assert-True ($state.resolved -ceq $expected) 'System appearance disagrees with the current OS theme'
    } else { Assert-True ($state.resolved -ceq $Preference) 'Fixed appearance was not applied' }
    Assert-AppearanceViews $state
    return $state
}

function Test-NativeAppearance {
    [void](Wait-Until { Select-NativeView 'content' } 'initial native appearance')
    $initial = Read-NativeAppearance
    Assert-True ($initial.preference -ceq 'system') 'Isolated native appearance did not default to System'
    foreach ($preference in @('light','dark','system')) {
        $state = Select-AppAppearance $preference
        Save-NativeWindow $script:NativeIdentity ("native-appearance-$preference.png")
    }
    $before = Read-NativeAppearance
    $rejected = Execute-AsyncJS @'
const done=arguments[arguments.length-1];
window.__TAURI_INTERNALS__.invoke('appearance',{preference:'invalid'}).then(()=>done(false),()=>done(true));
'@
    Assert-True ($rejected -eq $true) 'Native appearance accepted a malformed preference'
    $after = Read-NativeAppearance
    Assert-True ($after.preference -ceq $before.preference -and $after.resolved -ceq $before.resolved) 'Rejected appearance request changed the shared state'
    $wasMaximized = [RelayWindowProbe]::IsZoomed($script:NativeIdentity.Window)
    $denied = Execute-AsyncJS @'
const done=arguments[arguments.length-1];
window.__TAURI_INTERNALS__.invoke('frame_action',{action:'maximize'}).then(()=>done(false),()=>done(true));
'@
    Assert-True ($denied -eq $true) 'Controller content was allowed to invoke a privileged frame action'
    Assert-True ([RelayWindowProbe]::IsZoomed($script:NativeIdentity.Window) -eq $wasMaximized) 'Rejected frame action changed the native window'

    [void](Select-AppAppearance 'light')
    $saved = [IO.File]::ReadAllText((Join-Path $script:StateDirectory 'desktop\appearance.json')) | ConvertFrom-Json
    Assert-True ($saved.format -eq 1 -and $saved.preference -ceq 'light') 'Fixed appearance was not durably saved in the isolated state'
    $oldApp = $script:NativeIdentity
    Close-AppSession
    [void](Wait-Until {
        $old = Get-Process -Id $oldApp.Id -ErrorAction SilentlyContinue
        return $null -eq $old -or $old.StartTime.ToUniversalTime().Ticks -ne $oldApp.Started
    } 'owned app exits before appearance relaunch')
    Start-AppSession
    $launch = Fresh-Launch
    $controller = Controller-Identity $launch.pid
    Assert-True ($controller.Id -eq $script:ControllerIdentity.Id -and $controller.Started -eq $script:ControllerIdentity.Started) 'Appearance relaunch replaced the persistent controller'
    $restored = Read-NativeAppearance
    Assert-True ($restored.preference -ceq 'light' -and $restored.resolved -ceq 'light') 'Fixed appearance did not survive native close/reopen'
    Assert-True (Execute-JS 'return document.querySelector("select[aria-label=Appearance]")?.value === "light";') 'Reopened UI does not display the saved appearance'
    Assert-AppearanceViews $restored
    Save-NativeWindow $script:NativeIdentity 'native-appearance-reopened.png'
    Write-Host 'PASS: native UI appearance synchronizes controller/chrome/menu, persists across app restart, and rejects invalid or privileged IPC'
}

function Test-NormalLaunch {
    Close-AppSession
    $normalEnvironment = $TestEnvironment.Clone()
    $normalEnvironment.WEBVIEW2_USER_DATA_FOLDER = Join-Path $root 'normal-webview-profile'
    $script:NormalProcess = Start-OwnedProcess $Desktop @() $normalEnvironment
    $script:NativeIdentity = Wait-Until {
        $NormalProcess.Process.Refresh()
        if ($NormalProcess.Process.HasExited) { throw 'Normal native desktop launch exited unexpectedly' }
        $window = [RelayWindowProbe]::MainFrame($NormalProcess.Process.Id)
        if ($window -eq [IntPtr]::Zero) { return $null }
        $identity = Process-Identity $NormalProcess.Process.Id $Desktop
        $identity | Add-Member NoteProperty Window $window
        return $identity
    } 'normal native Windows window without WebDriver' 30
    $normalLaunch = Fresh-Launch
    Assert-True ($normalLaunch.pid -eq $ControllerIdentity.Id) 'Normal native launch replaced shared controller'
    [void](Wait-Until {
        $processes = @(Get-CimInstance Win32_Process | Select-Object ProcessId, ParentProcessId, Name, CommandLine)
        $descendants = @($NormalProcess.Process.Id)
        for ($round=0; $round -lt 6; $round++) {
            $children = @($processes | Where-Object { $_.ParentProcessId -in $descendants -and $_.ProcessId -notin $descendants })
            if ($children.Count -eq 0) { break }
            $descendants += @($children | ForEach-Object { [int]$_.ProcessId })
        }
        $owned = @($processes | Where-Object { $_.ProcessId -in $descendants })
        Assert-True (-not ($owned | Where-Object { $_.Name -eq 'wsl.exe' })) 'Normal native launch invoked WSL'
        $webviews = @($owned | Where-Object { $_.Name -eq 'msedgewebview2.exe' })
        if ($webviews.Count -eq 0) { return $false }
        Assert-True (-not ($webviews | Where-Object { $_.CommandLine -match '--(?:remote-debugging-port|enable-logging)(?:=|\s|$)' })) 'Normal native launch enabled automation or console logging'
        Assert-True ([RelayWindowProbe]::VisibleConsoles([int[]]$descendants) -eq 0) 'Normal native launch opened a console window'
        return $true
    } 'normal native WebView2 without debug or logging switches' 30)
    # Multi-WebView2 accessibility trees are not consistently exposed through
    # the process's first HWND. Inspect the visible top-level Relay frame and
    # retain a screenshot for visual review of normal (non-WebDriver) rendering.
    $response = [IntPtr]::Zero
    Assert-True ([RelayWindowProbe]::SendMessageTimeout($NativeIdentity.Window,0,[IntPtr]::Zero,[IntPtr]::Zero,2,1000,[ref]$response) -ne [IntPtr]::Zero) 'Normal native window is unresponsive'
    [void](Wait-Until { Save-NativeWindow $NativeIdentity 'native-normal-window.png' -CheckContent } 'normal native content paint' 30)
    [void]$NormalProcess.Process.CloseMainWindow()
    Assert-True ($NormalProcess.Process.WaitForExit(10000)) 'Normal native window did not close'
    Write-Host 'PASS: direct desktop launch uses native WebView2 without WSL, debugging switches or console window'
}

$Desktop = (Resolve-Path -LiteralPath $Desktop).Path
$Bundle = (Resolve-Path -LiteralPath $Bundle).Path
$Driver = (Resolve-Path -LiteralPath $Driver).Path
$Controller = Join-Path $Bundle 'relay-controller-windows-amd64.exe'
Assert-True (Test-Path -LiteralPath $Controller) 'Packaged native Windows controller is missing'
foreach ($binary in @($Desktop, $Controller)) {
    $file = [IO.File]::OpenRead($binary)
    try { Assert-True ($file.ReadByte() -eq 77 -and $file.ReadByte() -eq 90) 'Expected a native PE executable' }
    finally { $file.Dispose() }
}
$signature = Get-AuthenticodeSignature -LiteralPath $Driver
Assert-True ($signature.Status -eq 'Valid' -and $signature.SignerCertificate.Subject -match 'Microsoft Corporation') 'WebDriver must have a valid Microsoft Authenticode signature'
Assert-True (-not ($LaunchOnly -and $OverrideBundle)) 'LaunchOnly must use the extracted app default resources'
Assert-True (-not ($RenderingOnly -and ($LaunchOnly -or $FrameOnly -or $Clipboard))) 'RenderingOnly uses SSH and excludes physical-frame and clipboard modes'
$password = $null
if (-not ($FrameOnly -or $LaunchOnly)) {
    Assert-True (-not [string]::IsNullOrWhiteSpace($FixtureTarget)) 'Provide a disposable SSH fixture target'
    Assert-True ($FixtureFingerprint -match '^SHA256:[A-Za-z0-9+/]{40,}={0,2}$') 'Provide the known disposable fixture host fingerprint'
    $password = [IO.File]::ReadAllText((Resolve-Path -LiteralPath $FixturePasswordFile).Path).TrimEnd("`r", "`n")
    Assert-True ($password.Length -gt 0 -and $password.Length -lt 1024) 'Invalid disposable fixture password'
}
$root = Join-Path ([IO.Path]::GetTempPath()) ('relay-win-smoke-' + [Guid]::NewGuid().ToString('N'))
[void](New-Item -ItemType Directory -Path $root)
if (-not $Artifacts) { $Artifacts = Join-Path ([IO.Path]::GetTempPath()) ('relay-win-evidence-' + [Guid]::NewGuid().ToString('N')) }
[void](New-Item -ItemType Directory -Force -Path $Artifacts)
$Artifacts = (Resolve-Path -LiteralPath $Artifacts).Path
$StateDirectory, $RuntimeDirectory = (Join-Path $root 'controller'), (Join-Path $root 'unused-runtime')
$TestEnvironment = @{
    RELAY_DESKTOP_STATE_DIR=$StateDirectory; RELAY_DESKTOP_RUNTIME_DIR=$RuntimeDirectory;
    RELAY_DESKTOP_LOCAL='0'; RELAY_DESKTOP_BUNDLE=$null;
    RELAY_SSH_KNOWN_HOSTS=(Join-Path $root 'known_hosts');
    WEBVIEW2_USER_DATA_FOLDER=(Join-Path $root 'webview-profile')
    WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS=$null
}
if ($OverrideBundle) { $TestEnvironment.RELAY_DESKTOP_BUNDLE = $Bundle }
$listener = New-Object Net.Sockets.TcpListener([Net.IPAddress]::Loopback, 0)
$listener.Start(); $port = $listener.LocalEndpoint.Port; $listener.Stop()
$DriverURL = "http://127.0.0.1:$port"
$DriverHTTP, $BrowserHTTP = (New-HTTPClient), (New-HTTPClient $true)
$DriverProcess = $NativeIdentity = $ControllerIdentity = $SessionID = $null
$NormalProcess = $null
$remoteSession = $remoteHost = $null
$clock = [Diagnostics.Stopwatch]::StartNew()

try {
    $DriverProcess = Start-OwnedProcess $Driver @("--port=$port", '--allowed-ips=127.0.0.1') $TestEnvironment
    [void](Wait-Until { try { (HTTP $DriverHTTP GET "$DriverURL/status").Status -eq 200 } catch { $false } } 'Microsoft Edge WebDriver startup')
    Start-AppSession
    if (-not ($LaunchOnly -or $RenderingOnly)) { Test-CustomFrame }
    Test-NativeResizeGeometry
    $launch = Fresh-Launch
    $ControllerIdentity = Controller-Identity $launch.pid
    $Origin = $launch.address
    $authenticated = HTTP $BrowserHTTP GET $launch.url
    Assert-True ($authenticated.Status -eq 200) 'Independent browser could not authenticate'
    $CSRF = (API-JSON '/api/bootstrap').csrf
    Assert-True ((HTTP $BrowserHTTP GET $launch.url).Status -eq 401) 'One-time native login link was reused'
    Assert-True (@((API-JSON '/api/state').hosts).Count -eq 0) 'Test did not start with isolated Windows controller state'
    [void](Wait-Until { Body-Contains 'Connect your first machine' } 'empty isolated Windows fleet')
    Write-Host 'PASS: native PE + Win32 window + WebView2, native controller and independent browser login'
    if ($LaunchOnly) {
        Test-NativeAppearance
        [void](Wait-Until { Select-NativeView 'menu' } 'trusted portable menu')
        Assert-True (Execute-JS 'return JSON.stringify([...document.querySelectorAll("[data-action]")].map(button=>button.dataset.action)) === JSON.stringify(["reload","reconnect","close"]);') 'Portable app retained a browser-workspace action'
        [void](Wait-Until { Select-NativeView 'content' } 'portable fleet')
        Save-NativeWindow $NativeIdentity 'native-portable-fleet.png'
        Test-NormalLaunch
        Write-Host ('PASS: extracted portable resources and isolated launch; elapsed {0:N2} seconds' -f $clock.Elapsed.TotalSeconds)
        return
    }
    if ($FrameOnly) {
        Save-NativeWindow $NativeIdentity 'native-custom-frame.png'
        Write-Host ('Frame-only elapsed: {0:N2} seconds' -f $clock.Elapsed.TotalSeconds)
        return
    }

    Click-Button 'Connect your first machine'
    Fill-Element xpath "//label[starts-with(normalize-space(.),'SSH destination')]/input" $FixtureTarget
    Fill-Element xpath "//label[starts-with(normalize-space(.),'Display name')]/input" 'Windows SSH fixture'
    Fill-Element xpath "//label[starts-with(normalize-space(.),'Port override')]/input" ([string]$FixturePort)
    Click-Button 'Connect machine'
    $trusted = $sentPassword = $false
    [void](Wait-Until {
        $hosts = @((API-JSON '/api/state').hosts)
        if ($hosts.Count -eq 0) { return $false }
        $script:remoteHost = $hosts[0].id
        if ($hosts[0].status -eq 'online') { return $true }
        if ($hosts[0].status -eq 'error') { throw 'Disposable SSH fixture connection failed' }
        $offered = Execute-JS 'return document.querySelector(".xterm-rows")?.textContent.match(/SHA256:[A-Za-z0-9+/]{43}(?=\.|\s|$)/)?.[0] || "";'
        if (-not $script:trusted -and $offered) {
            Assert-True ($offered -ceq $FixtureFingerprint) "Disposable SSH fixture offered $offered; expected $FixtureFingerprint. Trust was withheld."
            Send-Setup 'yes'; $script:trusted=$true
        } elseif (-not $script:sentPassword -and (Terminal-Contains ($FixtureTarget + "'s password:"))) {
            Assert-True $script:trusted 'Refusing fixture password before expected host fingerprint'
            Send-Setup $script:password; $script:sentPassword=$true
        }
        return $false
    } 'native Windows SSH trust, password and remote bootstrap' 90)
    Assert-True ($trusted -and $sentPassword) 'SSH fixture did not exercise expected host trust and password flow'
    Assert-True (-not (Terminal-Contains $password)) 'Disposable SSH password appeared in the setup terminal'
    Assert-True (Test-Path -LiteralPath $TestEnvironment.RELAY_SSH_KNOWN_HOSTS) 'Accepted SSH host key was not saved in the isolated known-hosts file'
    [void](Wait-Until { Body-Contains 'Open machine' } 'remote setup completion')
    Click-Button 'Open machine'
    [void](Wait-Until { Body-Contains 'New session' } 'connected remote workspace')
    Click-Button 'New session'
    Click-Selector '[aria-label="Browse folders"]'
    [void](Wait-Until { Execute-JS 'return !!document.querySelector("[aria-label=\"Open folder Relay folder smoke\"]");' } 'remote home directory browser')
    Click-Selector '[aria-label="Open folder Relay folder smoke"]'
    [void](Wait-Until { Execute-JS 'return !!document.querySelector("[aria-label=\"Open folder Nested project\"]");' } 'remote directory navigation')
    Save-NativeWindow $NativeIdentity 'native-folder-browser.png'
    Click-Selector '[aria-label="Parent folder"]'
    [void](Wait-Until { Execute-JS 'return !!document.querySelector("[aria-label=\"Open folder Relay folder smoke\"]");' } 'parent folder navigation')
    $projectInput = "//input[@id=//label[starts-with(normalize-space(.),'Project folder')]/@for]"
    Fill-Element xpath $projectInput ($FixtureDirectory + '/Relay fol')
    [void](Wait-Until { Execute-JS 'const entries=document.querySelectorAll(".folder-entry"); return entries.length===1 && entries[0].textContent.includes("Relay folder smoke");' } 'remote typed-path completion')
    Click-Selector '[aria-label="Open folder Relay folder smoke"]'
    [void](Wait-Until { Execute-JS 'return !!document.querySelector("[aria-label=\"Open folder Nested project\"]");' } 'directory with spaces')
    Click-Button 'Use this folder'
    $selected = Execute-JS 'return document.querySelector(".folder-input-row input").value;'
    Assert-True ($selected -ceq ($FixtureDirectory + '/Relay folder smoke')) 'Folder selection did not retain the exact remote path'
    Fill-Element xpath "//label[starts-with(normalize-space(.),'Workspace')]/input" 'Windows native smoke'
    Fill-Element xpath "//label[starts-with(normalize-space(.),'Session name')]/input" 'Windows desktop smoke'
    Click-Button 'Start session'
    [void](Wait-Until { Body-Contains 'Watching' } 'remote PTY native attachment')
    $sessions = @(API-JSON "/api/hosts/$remoteHost/runtime/sessions")
    Assert-True ($sessions.Count -eq 1 -and $sessions[0].title -eq 'Windows desktop smoke') 'Unexpected remote test session'
    Assert-True ($sessions[0].cwd -ceq $selected) 'Remote terminal started outside the selected project folder'
    Write-Host 'PASS: remote home/parent navigation, typed path suggestions, explicit folder selection and session cwd'
    $remoteSession = $sessions[0].id
    $inputPath = "/api/hosts/$remoteHost/runtime/sessions/$remoteSession/input"
    Assert-True (Execute-JS 'return !document.querySelector(".terminal-composer, textarea[aria-label=\"Message or command\"]");') 'Separate command composer still exists'
    Claim-Terminal
    Assert-True ((API $inputPath POST @{ data="printf 'RELAY_DENIED_INPUT\n'`r" }).Status -eq 409) 'Second client bypassed native writer lease'
    Test-DirectTerminalKeys $remoteSession
    if ($RenderingOnly) { Test-TerminalResizeRendering }
    if ($Clipboard) { Test-TerminalClipboard }
    else { Write-Host 'SKIP: real Windows clipboard gate requires -Clipboard and a disposable account clipboard' }
    Send-Terminal "printf 'RELAY_%s\n' 'WINDOWS_NATIVE_OK'"
    [void](Wait-Until { Terminal-Contains 'RELAY_WINDOWS_NATIVE_OK' } 'remote output rendered in native WebView2')
    Click-Button 'Release control'
    [void](Wait-Until { Body-Contains 'Watching' } 'native lease release')
    Assert-True ((API $inputPath POST @{ data="printf 'RELAY_%s\n' 'WINDOWS_BROWSER_OK'`r" }).Status -eq 204) 'Browser input failed after native lease release'
    [void](Wait-Until { Terminal-Contains 'RELAY_WINDOWS_BROWSER_OK' } 'independent browser input appears in native terminal')
    Assert-True (-not (Terminal-Contains 'RELAY_DENIED_INPUT')) 'Rejected second-client input executed in the remote shell'
    Claim-Terminal
    Save-Screenshot 'native-session.png'
    Save-NativeWindow $NativeIdentity
    Write-Host 'PASS: native Windows SSH bootstrap, remote shell, visible input and exclusive browser/native control'

    Close-NativeWindow
    Close-AppSession
    $stillRunning = Process-Identity $ControllerIdentity.Id $ControllerIdentity.Path
    Assert-True ($stillRunning.Started -eq $ControllerIdentity.Started) 'Closing native window replaced or stopped native controller'
    Assert-True ((@(API-JSON "/api/hosts/$remoteHost/runtime/sessions"))[0].status -eq 'running') 'Remote session did not survive native window closure'
    Start-AppSession
    $reopened = Fresh-Launch
    Assert-True ($reopened.pid -eq $ControllerIdentity.Id -and $reopened.address -eq $Origin) 'Reopening native app replaced shared controller'
    [void](Session-WD POST '/url' @{ url="$Origin/#host/$remoteHost/session/$remoteSession" })
    [void](Wait-Until { (Body-Contains 'Watching') -and (Terminal-Contains 'RELAY_WINDOWS_NATIVE_OK') -and (Terminal-Contains 'RELAY_WINDOWS_BROWSER_OK') } 'remote session screen restored after native reopen')
    Claim-Terminal
    Send-Terminal "printf 'RELAY_%s\n' 'WINDOWS_REOPEN_OK'"
    [void](Wait-Until { Terminal-Contains 'RELAY_WINDOWS_REOPEN_OK' } 'reopened native terminal input')
    Save-Screenshot 'native-reopened.png'
    Close-NativeWindow
    Close-AppSession
    Assert-True ((API "/api/hosts/$remoteHost/disconnect" POST).Status -eq 204) 'Could not disconnect disposable SSH transport'
    Start-AppSession
    [void](Wait-Until { @((API-JSON '/api/state').hosts)[0].status -eq 'connecting' } 'app startup automatically reconnects the saved host')
    [void](Session-WD POST '/url' @{ url="$Origin/#host/$remoteHost" })
    [void](Wait-Until { Body-Contains 'Open setup' } 'saved-host authentication attention')
    Click-Button 'Open setup'
    [void](Wait-Until { Terminal-Contains ($FixtureTarget + "'s password:") } 'saved-host password prompt without another Connect action')
    Assert-True (-not (Terminal-Contains 'Are you sure')) 'Saved host trust was lost'
    Send-Setup $password
    [void](Wait-Until { @((API-JSON '/api/state').hosts)[0].status -eq 'online' } 'automatic saved-host reconnect completes')
    [void](Wait-Until { Body-Contains 'Open machine' } 'automatic reconnect setup completion')
    Click-Button 'Open machine'
    [void](Session-WD POST '/url' @{ url="$Origin/#host/$remoteHost/session/$remoteSession" })
    [void](Wait-Until { Terminal-Contains 'RELAY_WINDOWS_REOPEN_OK' } 'same remote session survives transport disconnect and startup reconnect')
    Write-Host 'PASS: app startup reconnects saved hosts, retains trust, requests unsaved credentials, and restores the original remote session'
    Assert-True ((API "/api/hosts/$remoteHost/runtime/sessions/$remoteSession" DELETE).Status -eq 204) 'Could not remove disposable remote session'
    $remoteSession=$null
    Write-Host 'PASS: native close/reopen preserves native controller, SSH transport, remote PTY and terminal output'

    Test-NormalLaunch
    Write-Host ('Elapsed: {0:N2} seconds; evidence: {1}' -f $clock.Elapsed.TotalSeconds, $Artifacts)
} catch {
    if ($LastNativeLaunchDiagnostic) {
        try {
            $LastNativeLaunchDiagnostic | ConvertTo-Json -Depth 8 | Set-Content -Encoding UTF8 -LiteralPath (Join-Path $Artifacts 'owned-launch-diagnostic.json')
        } catch { }
    }
    if ($SessionID) { try { Save-FrameTrace } catch { }; try { Save-Screenshot 'failure.png' } catch { } }
    if ($NativeIdentity) { try { Save-NativeWindow $NativeIdentity 'failure-native-window.png' } catch { } }
    Write-Host "Native Windows failure evidence: $Artifacts"
    throw
} finally {
    if ($remoteSession -and $remoteHost -and $Origin) {
        try { [void](API "/api/hosts/$remoteHost/runtime/sessions/$remoteSession" DELETE) } catch { }
    }
    Close-AppSession
    Stop-Identity $NativeIdentity
    if (-not $ControllerIdentity) {
        # Capture an app-started controller even if WebView startup failed. The
        # exact executable plus this random private state path scope ownership.
        $releaseRoot = (Join-Path $StateDirectory 'controller-releases') + '\'
        $orphans = @(Get-CimInstance Win32_Process | Where-Object {
            ([string]::Equals($_.ExecutablePath, $Controller, [StringComparison]::OrdinalIgnoreCase) -or
                ($_.ExecutablePath -and $_.ExecutablePath.StartsWith($releaseRoot, [StringComparison]::OrdinalIgnoreCase))) -and
            $_.CommandLine.Contains($StateDirectory)
        })
        foreach ($orphan in $orphans) {
            try { Stop-Identity (Controller-Identity $orphan.ProcessId) } catch { }
        }
    }
    Stop-Identity $ControllerIdentity
    Stop-OwnedProcess $NormalProcess
    Stop-OwnedProcess $DriverProcess
    $DriverHTTP.Dispose(); $BrowserHTTP.Dispose()
    # Only this test's directory is removed; no USERPROFILE/HOME override exists.
    Remove-Item -LiteralPath $root -Recurse -Force -ErrorAction SilentlyContinue
    $password=$null
}
