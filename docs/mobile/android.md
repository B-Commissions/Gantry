# Android builds

Gantry runs the same Go app on a phone: the Go server cross-compiles for Android and runs on-device as a child process, and a thin generated Kotlin shell points a full-screen WebView at it on `127.0.0.1`. There is no gomobile and no Java in your project - the whole Android side is synthesized into `.gantry/android/` on every build, the same way the Vite root is. This page is about building and running on Android; home-screen [Widgets](widgets.md) and system [Notifications](notifications.md) each have their own page.

![gantry mobile dev android checks the toolchain, finds the one USB device, builds an APK for its ABI, installs and launches it, then streams the app's logcat under the gantry-go tag.](android-loop.svg)

## Run it on your phone

`gantry mobile dev android` is the phone equivalent of `gantry dev`. It checks the toolchain (and **offers to install** what's missing), finds the one USB device, builds the APK for that device's ABI, installs it, launches `<your.id>/.MainActivity`, clears the old log with `adb logcat -c` and then streams the app's logcat (tag `gantry-go`) into your terminal until Ctrl+C:

```
gantry mobile dev android
```

The command needs a `mobile` section with an `id` in `gantry.json` (see [Configuration](#configuration)) or it stops before touching the toolchain. Under the hood the stream is `adb logcat -v time -s gantry-go` - the same command you can run yourself against an installed build.

### The device has to be plugged in

The phone must be **plugged in over USB** with USB debugging enabled (Settings > Developer options). adb-over-wifi is deliberately unsupported - the cable is what keeps logcat reliable. Gantry inspects `adb devices -l` and classifies each entry: a serial containing `:` (an `adb connect` ip:port) or starting with `adb-` (an mDNS wireless-debugging serial) counts as network and is rejected; anything else in the `device` state is treated as USB. Local emulators count as plugged in. The failure messages are specific:

- **no device found** - plug in and enable USB debugging.
- **device ... is unauthorized** - accept the USB debugging prompt on the phone.
- **only wifi/network adb devices found** - a wireless connection is not enough; use the cable.
- **multiple devices connected (...)** - unplug the extras or set `ANDROID_SERIAL`.

The device's ABI (`adb shell getprop ro.product.cpu.abi`) picks the build arch: `arm64-v8a` builds `android/arm64`, `x86_64` builds `android/amd64`. Any other ABI is an error.

## Build an installable APK

`gantry build --targets android` packs your app into an installable APK - the Go server plus the built frontend in one file:

```
gantry build --targets android          # arm64 (bare "android" defaults to arm64)
gantry build --targets android/arm64,android/amd64   # multi-ABI APK (amd64 = emulators)
adb install -r dist/android/myapp-0.1.0.apk
```

The finished APK lands at `dist/android/<name>-<version>.apk` (release, signed with your keystore or a debug key). All the android arches in one `gantry build` fold into a single multi-ABI APK. You can also list the targets once in `gantry.json` under `build.targets` and just run `gantry build`.

Unlike the dev loop, `gantry build` does not install missing tools: when a toolchain piece is absent the android target prints a coloured warning naming the exact fix and is skipped, while the other targets in the same run still build. Misconfiguration - a missing `mobile.id`, an unknown permission name, a `version` that is not `X.Y.Z` semver - *is* a hard failure: that is yours to fix.

## Configuration

The `mobile` section of `gantry.json` carries the app's identity and per-OS options; the android and ios targets both require it.

```json
"mobile": {
  "id": "ec.morrison.myapp",             // required: reverse-DNS app id - changing it after release makes stores treat it as a different app
  "permissions": ["notifications", "camera"],
  "android": {
    "minSdk": 26,                        // default 26 (Android 8)
    "targetSdk": 35,                     // default 35 (Android 15)
    "keystore": {                        // omit = debug-signed (installable, not store-ready)
      "file": "release.jks",             // relative to the app root
      "alias": "myapp",
      "passwordEnv": "MYAPP_KEY_PASS"    // env var read at build time - the password never lives in the repo
    }
  }
}
```

`minSdk` and `targetSdk` default to 26 and 35 when omitted. The Android `versionCode` derives from the top-level `version`: `major*10000 + minor*100 + patch`, so `0.1.0` is `100` and every semver bump yields a strictly larger code. Omitting the whole `android` block is fine - you get the defaults and a debug-signed APK.

## Permissions

Friendly names, mapped to real Android permissions at build time. `INTERNET` is always included first (the app talks to its own local server), then your names in declaration order, deduplicated. The "dangerous" (Runtime) permissions are also requested with a one-shot runtime prompt on first launch - the shell remembers it asked and never re-prompts.

| Name | Android permission(s) | Runtime prompt |
| --- | --- | --- |
| `camera` | CAMERA | yes |
| `microphone` | RECORD_AUDIO | yes |
| `location` | ACCESS_FINE_LOCATION, ACCESS_COARSE_LOCATION | yes |
| `location-background` | ACCESS_BACKGROUND_LOCATION | yes |
| `notifications` | POST_NOTIFICATIONS | yes |
| `files` | READ_EXTERNAL_STORAGE, WRITE_EXTERNAL_STORAGE | yes |
| `bluetooth` | BLUETOOTH_CONNECT, BLUETOOTH_SCAN | yes |
| `contacts` | READ_CONTACTS | yes |
| `vibrate` | VIBRATE | no |
| `boot` | RECEIVE_BOOT_COMPLETED | no |
| `network-state` | ACCESS_NETWORK_STATE | no |
| `foreground-service` | FOREGROUND_SERVICE | no |

An unknown name is a build error listing the whole valid vocabulary. Web-side `getUserMedia` calls inside the WebView are granted automatically when the app already holds the matching Android permission (the shell maps a `RESOURCE_VIDEO_CAPTURE` request to `CAMERA`, `RESOURCE_AUDIO_CAPTURE` to `RECORD_AUDIO`, and grants only what is already held). The same friendly names carry over to [iOS](ios.md), translated to Info.plist usage strings.

## What the machine needs

`gantry mobile dev android` offers to fix the SDK, its command-line tools, adb (platform-tools) and the NDK; `gantry build` expects them to be present and skips the target with a warning otherwise. A JDK 17+ is the one piece nothing will install for you.

| Piece | Where gantry looks | Fix when missing |
| --- | --- | --- |
| Android SDK | `ANDROID_HOME`, `ANDROID_SDK_ROOT`, then the Android Studio default (`%LOCALAPPDATA%\Android\Sdk`, `~/Library/Android/sdk`, `~/Android/Sdk`) | install Android Studio or the cmdline-tools |
| Android NDK | `ANDROID_NDK_HOME`, else the newest version under `<sdk>/ndk/` | `sdkmanager "ndk;27.2.12479018"` |
| JDK 17+ | `JAVA_HOME`, else `java` on PATH (its major version is read from `<home>/release`, and 17+ required) | install any JDK 17+ |

On a bare machine the dev loop downloads Google's command-line tools zip (~150 MB, version `13114758`) into `<sdk>/cmdline-tools/latest`, accepts the SDK licenses, then runs `sdkmanager` to fetch `platform-tools` and `ndk;27.2.12479018`. Detection accepts *any* NDK version present - the pinned version is only what the install hint fetches. Gradle itself is **not** on the list: the generated project ships the Gradle wrapper (`gradlew`), which downloads Gradle on the first `:app:assembleRelease`.

## Icons

The launcher icon renders from `icons/icon.png` (or your `icons` directory) scaled to every mipmap density: `mdpi` 48px, `hdpi` 72px, `xhdpi` 96px, `xxhdpi` 144px, `xxxhdpi` 192px, each written as `ic_launcher.png`. No icon file = the same placeholder glyph the desktop surfaces draw. The launcher icon is also the notification small icon - see [Notifications > Icons](notifications.md#icons).

## Notes (advanced)

### How the shell behaves

The generated Kotlin shell is a thin supervisor around the Go server; you never touch it, but knowing what it does explains the app's behaviour on-device:

- The server is packed into the APK as `jniLibs/<abi>/libgantryapp.so` (Android extracts a jniLib executable into `nativeLibraryDir`) and spawned with `--port 0 --no-open --announce-ready --token <32-hex>`. It binds an ephemeral port and prints `GANTRY_READY port=<n>` on stdout; the shell reads that to point the WebView at `http://127.0.0.1:<n>/?gantry_token=<token>`.
- The token is 16 random bytes per launch. Only the shell's own WebView carries it, so other apps on the phone hitting the loopback port get 403 (the runtime's token guard sets a `gantry_token` cookie on the first load and requires it thereafter).
- The server runs with `HOME` = the app's private files dir and `TMPDIR` = its cache dir - the same `$HOME` a [widget](widgets.md) refresh reads.
- The server's stdout streams to logcat under tag `gantry-go`: `adb logcat -s gantry-go`. Control lines (`GANTRY_NOTIFY ...`) are executed by the shell rather than logged - see [Notifications](notifications.md).
- The window is edge-to-edge (Android 15 enforces it): the WebView canvas is padded inside the status/navigation bars and above the keyboard (`adjustResize`), and the inset strips are painted with the page's own background colour (sampled from `getComputedStyle(document.body).backgroundColor`), so the app *looks* like it runs under the bars without content ever hiding there. System bar icons flip dark/light to match the sampled luminance.
- If the server dies it is restarted with exponential backoff (500 ms, doubling to a 15 s cap; a run that lasted 30 s resets the backoff), and the WebView re-points itself at the new port.
- The back gesture/button walks WebView history (`web.canGoBack()`) before leaving the app.

The same debuggable build is what `gantry test --device android` installs, so it exposes the WebView devtools socket and honours the runner's port/token/env; release builds never do.

### Customizing the Android project

`.gantry/android/` is regenerated every build - never edit it. Instead, create `mobile/android/` in your app root: after synthesis its contents are copied over the generated tree, path for path. `mobile/android/app/src/main/res/values/strings.xml` replaces the generated one, extra Kotlin files land in the source set, and so on. Gradle's caches live inside `.gantry/android/` and survive between runs (the synth overwrites files in place rather than wiping the directory).

Next: [Widgets](widgets.md) and [Notifications](notifications.md) - the two mobile surfaces that build on the Android shell.
