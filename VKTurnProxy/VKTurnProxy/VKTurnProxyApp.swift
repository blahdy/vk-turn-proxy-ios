import SwiftUI

/// Tiny inbox that forwards an incoming `vkturnproxy://import?data=…`
/// URL from the App's `.onOpenURL` (which fires reliably on cold and
/// warm launches at the WindowGroup level) into SettingsView, where
/// the parse + confirm + apply flow lives. SettingsView observes
/// `pendingURL` via @StateObject and consumes it on .onAppear AND
/// .onChange, so the URL is acted on whether SettingsView was already
/// mounted at the moment of delivery or only mounted later when the
/// user navigates to it.
@MainActor
final class ConnectionLinkInbox: ObservableObject {
    static let shared = ConnectionLinkInbox()
    @Published var pendingURL: URL?
    private init() {}
}

@main
struct VKTurnProxyApp: App {
    init() {
        // Version comes from Bundle's CFBundleVersion = $(CURRENT_PROJECT_VERSION)
        // (per project.yml info.properties). Both main app and PacketTunnel
        // extension log their own build number on startup so post-mortem log
        // analysis can immediately tell whether the running binary matches
        // the source git state — earlier confusion (2026-05-10) was caused
        // by an extension running stale Go code from a not-rebuilt xcframework
        // while the source had moved on.
        let build = Bundle.main.object(forInfoDictionaryKey: "CFBundleVersion") as? String ?? "?"
        SharedLogger.shared.log("[App] VKTurnProxy launched (build \(build))")
    }

    var body: some Scene {
        WindowGroup {
            ContentView()
                // Capture vkturnproxy:// AND wdtt:// URLs at the WindowGroup
                // level so cold-launch via URL-tap works regardless of which
                // page SettingsView is currently on. wdtt:// is amurcanov's
                // proxy-turn-vk-android link scheme (SRTP-WRAP-A interop) — his
                // own app doesn't register it, so we're the sole handler. Any
                // other scheme is ignored here.
                .onOpenURL { url in
                    let scheme = url.scheme?.lowercased()
                    if scheme == "vkturnproxy" || scheme == "wdtt" {
                        ConnectionLinkInbox.shared.pendingURL = url
                    }
                }
        }
    }
}
