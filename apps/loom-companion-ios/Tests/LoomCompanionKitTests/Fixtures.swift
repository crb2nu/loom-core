import Foundation

/// Anchor for `Bundle(for:)` when the tests run from the Xcode project rather
/// than SwiftPM. `Bundle.module` is synthesised only by SwiftPM, so the Xcode
/// unit-test target (LoomCompanionKitTests in project.yml) needs this fallback
/// — the Fixtures directory is added there as a folder reference so the
/// `subdirectory: "Fixtures"` lookup resolves in both builds.
private final class FixtureBundleAnchor {}

private var fixtureBundle: Bundle {
    #if SWIFT_PACKAGE
    return Bundle.module
    #else
    return Bundle(for: FixtureBundleAnchor.self)
    #endif
}

/// Load a JSON fixture file from the test bundle.
func loadFixture(_ name: String) throws -> Data {
    guard let url = fixtureBundle.url(forResource: name, withExtension: "json", subdirectory: "Fixtures") else {
        throw FixtureError.notFound(name)
    }
    return try Data(contentsOf: url)
}

enum FixtureError: Error {
    case notFound(String)
}
