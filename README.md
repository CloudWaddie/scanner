# simple scanner
[![Made by Human](https://madebyhuman.iamjarl.com/badges/made-white.svg)](https://madebyhuman.iamjarl.com)

a go based scanner that uses regex to scan github for stuff.

example config:
```
"github_token" = "REDACTED"
[signatures]
"Private Key"  = '-----BEGIN PRIVATE KEY-----'
"Private RSA Key"  = '-----BEGIN RSA PRIVATE KEY-----'
"Private ECSDA Key"  = '-----BEGIN EC PRIVATE KEY-----'
"Private Ed25519 Key"  = '-----BEGIN ED25519 PRIVATE KEY-----'
```