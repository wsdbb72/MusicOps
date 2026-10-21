# MusicOps maintenance

- Keep the desktop app, Chinese README, configuration help and packaged behavior consistent.
- For each completed batch of changes, commit and push the reviewed source to `wsdbb72/MusicOps`, increment the app version, and publish a matching GitHub Release with the Windows installer, portable ZIP, macOS Apple Silicon native PKG installer and application ZIP, source ZIP and SHA-256 checksums. Build and smoke-test the macOS applications and real installer/reinstall behavior on GitHub Actions because the maintainer has no local Mac.
- Use only `MusicOps X.Y.Z` as the release title, with the matching version number.
- Use the complete desktop release workflow on a `codex/release-*` branch to build and verify Windows and native Apple Silicon macOS before publishing. Synchronize the same verified commit to `main`; never publish an incomplete set or replace an existing public version.
- Run relevant regression tests and verify the actual packaged EXE before publishing. Clearly distinguish synthetic verification from authenticated live-site verification.
- Keep cookies, tokens, passwords, private service URLs, personal profiles, music files and local diagnostic records out of Git and public release assets. Credentials must remain editable in the setup wizard and encrypted locally.
- Preserve existing profiles, credentials and configured media tools during upgrades. Do not initiate real downloads or library changes merely to smoke-test a release.
- Use the shared brand assets for the desktop window, EXE, installer and README. Update screenshots when the interface materially changes.

- From 0.4.3 onward, build and publish only arm64 for macOS. Preserve historical Intel release attachments; do not build new Intel artifacts. Automatic updates use only official GitHub release installers, require SHA-256 verification and safely stop the worker before installer handoff.
- Keep the persistent local album catalogue incremental; playlist presence must verify unchanged existing audio, and local tag grouping must not be presented as confirmed online release identity.
- Also build and smoke-test the Linux amd64 browser desktop container on GitHub Actions. Publish the exact tested image to `wsdbb72/muiscops` using the encrypted Actions secret, and promote `latest` only after the matching desktop Release succeeds. Preserve `/config` and sibling media storage, require browser authentication, and keep Docker deployment instructions current.
