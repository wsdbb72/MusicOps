# MusicOps maintenance

- Keep the desktop app, Chinese README, configuration help and packaged behavior consistent.
- For each completed batch of changes, commit and push the reviewed source to `wsdbb72/MusicOps`, increment the app version, and publish a matching GitHub Release with the Windows installer, portable ZIP, macOS Apple Silicon and Intel native PKG installers and application ZIPs, source ZIP and SHA-256 checksums. Build and smoke-test the macOS applications and real installer/reinstall behavior on GitHub Actions because the maintainer has no local Mac.
- Run relevant regression tests and verify the actual packaged EXE before publishing. Clearly distinguish synthetic verification from authenticated live-site verification.
- Keep cookies, tokens, passwords, private service URLs, personal profiles, music files and local diagnostic records out of Git and public release assets. Credentials must remain editable in the setup wizard and encrypted locally.
- Preserve existing profiles, credentials and configured media tools during upgrades. Do not initiate real downloads or library changes merely to smoke-test a release.
- Use the shared brand assets for the desktop window, EXE, installer and README. Update screenshots when the interface materially changes.
