# Connecting to a TrueNAS

How the address, the certificate and the API key are used. The settings themselves are listed in the [README](../README.md#settings).

## The certificate

A TrueNAS comes with a certificate it signed itself and made out to `localhost`. Nothing trusts it unasked, and it can never match the name the NAS is reached by.

**Hand over the NAS's own certificate.** Save it to a file (in the TrueNAS interface: Credentials, then Certificates, then download) and pass it with `--certificate-file` (`TRUENAS_CERTIFICATE_FILE`). The NAS is then known by that certificate's key, whatever name and dates are on it. Renewing the certificate with the same key changes nothing. A new key is refused until the file is replaced.

**Or give the NAS a proper certificate.** One made out to its own name needs nothing, or `--ca-file` (`TRUENAS_CA_FILE`) for an authority of your own.

**Or switch the check off.** `--skip-certificate-check` (`TRUENAS_SKIP_CERTIFICATE_CHECK=true`) takes whatever certificate is shown, which means nothing proves it is your NAS that answers.

TrueNAS revokes an API key that is ever sent without TLS, so an `http://` address is refused before anything is sent.

## On the NAS itself

Run on the NAS, the address can be the NAS's own socket: `--server unix://`, or `unix:///run/middleware/middlewared.sock` in full. No key is needed. The NAS can see which user is asking, and takes root or any user it has an account for.

## How the key is used

**TrueNAS 26 and later: the key is never sent.** The program and the NAS each prove to the other that they hold the key. Nothing that listens learns it, and nothing that answers in the NAS's place does either. The login is also tied to the certificate the NAS showed, so it cannot be passed along by something sitting in the middle. This is used whenever the NAS offers it, and it needs the user name.

**Before TrueNAS 26: the key is sent, over TLS.** That is the only login those versions have. Because of it, the key is not sent to a NAS whose certificate was not checked. With `--skip-certificate-check` an older NAS is refused, with a message saying why, unless `--send-key-unchecked` says to send it anyway. Without that rule, something in the middle of a newer NAS's connection could claim to be an older one and be handed the key.

**A NAS behind a reverse proxy** shows the proxy's certificate, not its own, so a login tied to the certificate is refused even with the right key. `--no-channel-binding` leaves the login untied. The key is still never sent.

**The key can come from a file** with `--token-file` (`TRUENAS_TOKEN_FILE`): the key on a line of its own, or a key file as TrueNAS hands one out. A key file that holds only the parts worked out of a key, and not the key, works with TrueNAS 26 and later.

**Without a user name** the key is logged in the older way, which TrueNAS 27 removes.

## Logging

`TRUENAS_LOG` sets how much is logged: `INFO`, `DEBUG`, or `TRACE` for every message, with its secrets hidden.
