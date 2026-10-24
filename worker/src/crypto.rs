use aes_gcm::aead::{Aead, KeyInit, Payload};
use aes_gcm::{Aes256Gcm, Nonce};
use anyhow::{Context, Result, bail};
use base64::Engine;
use base64::engine::general_purpose::STANDARD as BASE64_STD;
use hmac::{Hmac, Mac};
use sha2::Sha256;
use uuid::Uuid;

type HmacSha256 = Hmac<Sha256>;

/// Builds the canonical AES-256-GCM Additional Authenticated Data (AAD)
/// matching Go's `secrets.AAD(tenantID, endpointID, secretID, keyVersion)`.
pub fn canonical_aad(
    tenant_id: Uuid,
    endpoint_id: Uuid,
    secret_id: Uuid,
    key_version: i32,
) -> String {
    format!("{tenant_id}:{endpoint_id}:{secret_id}:{key_version}")
}

/// Decodes a `whsec_<base64>` signing secret string into its 32-byte raw HMAC key.
pub fn decode_whsec(encoded: &str) -> Result<[u8; 32]> {
    let b64 = encoded
        .strip_prefix("whsec_")
        .context("signing secret must start with whsec_")?;
    let decoded = BASE64_STD
        .decode(b64)
        .context("signing secret base64 decode failed")?;
    let key: [u8; 32] = decoded
        .try_into()
        .map_err(|_| anyhow::anyhow!("decoded signing secret must be 32 bytes"))?;
    Ok(key)
}

/// Decrypts an `endpoint_secrets` row using AES-256-GCM and extracts the 32-byte HMAC key.
pub fn decrypt_endpoint_secret(
    kek: &[u8; 32],
    ciphertext: &[u8],
    nonce: &[u8],
    tenant_id: Uuid,
    endpoint_id: Uuid,
    secret_id: Uuid,
    key_version: i32,
) -> Result<[u8; 32]> {
    let nonce_arr: [u8; 12] = nonce
        .try_into()
        .map_err(|_| anyhow::anyhow!("invalid nonce length {}", nonce.len()))?;
    let cipher = <Aes256Gcm as KeyInit>::new_from_slice(kek).context("init AES-256-GCM")?;
    let aad = canonical_aad(tenant_id, endpoint_id, secret_id, key_version);
    let plaintext = cipher
        .decrypt(
            &Nonce::from(nonce_arr),
            Payload {
                msg: ciphertext,
                aad: aad.as_bytes(),
            },
        )
        .map_err(|_| anyhow::anyhow!("AES-256-GCM authentication failed"))?;

    if let Ok(as_str) = std::str::from_utf8(&plaintext)
        && as_str.starts_with("whsec_")
    {
        return decode_whsec(as_str);
    }
    if plaintext.len() == 32 {
        let mut out = [0u8; 32];
        out.copy_from_slice(&plaintext);
        return Ok(out);
    }
    bail!("decrypted secret has unexpected format")
}

/// Computes a single Standard Webhooks `v1,<base64>` signature over
/// `webhook_id + "." + timestamp + "." + raw_body` per `contracts/signature-v1.md`.
pub fn sign_v1(
    secret_key: &[u8; 32],
    webhook_id: &str,
    timestamp_secs: i64,
    raw_body: &[u8],
) -> Result<String> {
    if webhook_id.is_empty() || webhook_id.contains('.') {
        bail!("webhook-id must be non-empty and must not contain '.'");
    }
    let ts_str = timestamp_secs.to_string();
    let mut mac =
        <HmacSha256 as Mac>::new_from_slice(secret_key).context("initialize HMAC-SHA256")?;
    mac.update(webhook_id.as_bytes());
    mac.update(b".");
    mac.update(ts_str.as_bytes());
    mac.update(b".");
    mac.update(raw_body);
    let tag = mac.finalize().into_bytes();
    Ok(format!("v1,{}", BASE64_STD.encode(tag)))
}

/// Computes the `webhook-signature` header for one or more active secrets (joined by space).
pub fn sign_webhook_header(
    secrets: &[[u8; 32]],
    webhook_id: &str,
    timestamp_secs: i64,
    raw_body: &[u8],
) -> Result<String> {
    if secrets.is_empty() {
        bail!("at least one active secret is required");
    }
    let mut parts = Vec::with_capacity(secrets.len());
    for key in secrets {
        parts.push(sign_v1(key, webhook_id, timestamp_secs, raw_body)?);
    }
    Ok(parts.join(" "))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn matches_published_signature_v1_vector() {
        let key = decode_whsec("whsec_AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=")
            .expect("decode test secret");
        for (i, b) in key.iter().enumerate() {
            assert_eq!(*b, i as u8);
        }

        let event_id = "msg_01J9Z6Q3V8T7R2M4N5P6Q7R8S9";
        let timestamp = 1_700_000_000i64;
        let raw_body = br#"{"type":"order.created","data":{"id":"ord_123","amount":4200}}"#;

        let sig = sign_v1(&key, event_id, timestamp, raw_body).expect("sign vector");
        assert_eq!(sig, "v1,zWH/4qANrUYMGmZOIkwS0phhFBRxMelsoEkhyx2G71Q=");

        // Mutating 1 byte of the body must produce a different signature
        let tampered_body = br#"{"type":"order.created","data":{"id":"ord_123","amount":4201}}"#;
        let tampered_sig =
            sign_v1(&key, event_id, timestamp, tampered_body).expect("sign tampered");
        assert_ne!(sig, tampered_sig);
    }

    #[test]
    fn multi_secret_rotation_header_joins_with_space() {
        let k1 = [1u8; 32];
        let k2 = [2u8; 32];
        let body = b"{\"ok\":true}";
        let header = sign_webhook_header(&[k1, k2], "evt_123", 1_700_000_000, body)
            .expect("multi-secret sign");
        let parts: Vec<&str> = header.split(' ').collect();
        assert_eq!(parts.len(), 2);
        assert!(parts[0].starts_with("v1,"));
        assert!(parts[1].starts_with("v1,"));
        assert_ne!(parts[0], parts[1]);
    }

    #[test]
    fn aes_gcm_decrypt_verifies_aad() {
        let kek = [0x41u8; 32];
        let tenant_id = Uuid::new_v4();
        let endpoint_id = Uuid::new_v4();
        let secret_id = Uuid::new_v4();
        let key_version = 1;

        let raw_secret = [0x55u8; 32];
        let whsec = format!("whsec_{}", BASE64_STD.encode(raw_secret));
        let aad = canonical_aad(tenant_id, endpoint_id, secret_id, key_version);

        let cipher = <Aes256Gcm as KeyInit>::new_from_slice(&kek).unwrap();
        let nonce = [0x07u8; 12];
        let ciphertext = cipher
            .encrypt(
                &Nonce::from(nonce),
                Payload {
                    msg: whsec.as_bytes(),
                    aad: aad.as_bytes(),
                },
            )
            .unwrap();

        let opened = decrypt_endpoint_secret(
            &kek,
            &ciphertext,
            &nonce,
            tenant_id,
            endpoint_id,
            secret_id,
            key_version,
        )
        .expect("decrypt with matching AAD");
        assert_eq!(opened, raw_secret);

        // Wrong tenant_id must fail authentication
        let wrong_tenant = Uuid::new_v4();
        assert!(
            decrypt_endpoint_secret(
                &kek,
                &ciphertext,
                &nonce,
                wrong_tenant,
                endpoint_id,
                secret_id,
                key_version,
            )
            .is_err()
        );
    }
}
