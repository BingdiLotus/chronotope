#!/usr/bin/env python3
"""AWS SigV4 签名的 S3 GET（无 boto3 依赖——RustFS S3 网关直读）。"""
import sys, hashlib, hmac, datetime, urllib.request, urllib.parse

def sign(key, msg):
    return hmac.new(key, msg.encode(), hashlib.sha256).digest()

endpoint, bucket, key, access, secret = sys.argv[1:6]
out_path = sys.argv[6] if len(sys.argv) > 6 else None
now = datetime.datetime.utcnow()
amzdate = now.strftime('%Y%m%dT%H%M%SZ')
datestamp = now.strftime('%Y%m%d')
region = 'us-east-1'
service = 's3'
host = urllib.parse.urlparse(endpoint).netloc
canonical_uri = '/' + '/'.join(urllib.parse.quote(c, safe='-_.~/') for c in [bucket, key])
canonical_querystring = ''
canonical_headers = f'host:{host}\nx-amz-content-sha256:UNSIGNED-PAYLOAD\nx-amz-date:{amzdate}\n'
signed_headers = 'host;x-amz-content-sha256;x-amz-date'
payload_hash = 'UNSIGNED-PAYLOAD'
canonical_request = '\n'.join(['GET', canonical_uri, canonical_querystring, canonical_headers, signed_headers, payload_hash])
scope = f'{datestamp}/{region}/{service}/aws4_request'
string_to_sign = '\n'.join(['AWS4-HMAC-SHA256', amzdate, scope, hashlib.sha256(canonical_request.encode()).hexdigest()])
k = sign(('AWS4' + secret).encode(), datestamp)
k = sign(k, region)
k = sign(k, service)
k = sign(k, 'aws4_request')
signature = hmac.new(k, string_to_sign.encode(), hashlib.sha256).hexdigest()
auth = f'AWS4-HMAC-SHA256 Credential={access}/{scope}, SignedHeaders={signed_headers}, Signature={signature}'
req = urllib.request.Request(f'{endpoint}/{bucket}/{key}', headers={
    'Authorization': auth, 'x-amz-date': amzdate, 'x-amz-content-sha256': 'UNSIGNED-PAYLOAD'})
with urllib.request.urlopen(req, timeout=30) as r:
    data = r.read()
if out_path:
    open(out_path, 'wb').write(data)
else:
    sys.stdout.buffer.write(data)
