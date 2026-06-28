"""Nexus Object Storage Client.

Provides an S3-compatible client with Nexus-specific extensions
for vector search, full-text search, and resumable uploads.
"""

import json
import os
from typing import BinaryIO, Dict, Iterator, List, Optional

import boto3
from botocore.config import Config


class NexusClient:
    """Client for interacting with Nexus Object Storage.

    Wraps boto3 S3 client with Nexus-specific extensions.
    """

    def __init__(
        self,
        endpoint_url: str,
        access_key: str,
        secret_key: str,
        region: str = "us-east-1",
    ) -> None:
        self.endpoint_url = endpoint_url
        self.region = region
        self.s3 = boto3.client(
            "s3",
            endpoint_url=endpoint_url,
            aws_access_key_id=access_key,
            aws_secret_access_key=secret_key,
            region_name=region,
            config=Config(signature_version="s3v4"),
        )

    # ---- Standard S3 Operations ----

    def put_object(
        self,
        bucket: str,
        key: str,
        body: BinaryIO,
        content_type: Optional[str] = None,
        metadata: Optional[Dict[str, str]] = None,
    ) -> dict:
        """Upload an object to a bucket."""
        kwargs = {
            "Bucket": bucket,
            "Key": key,
            "Body": body,
        }
        if content_type:
            kwargs["ContentType"] = content_type
        if metadata:
            kwargs["Metadata"] = metadata
        return self.s3.put_object(**kwargs)

    def get_object(self, bucket: str, key: str) -> dict:
        """Download an object from a bucket.

        Returns a dict with 'Body' stream and metadata.
        """
        return self.s3.get_object(Bucket=bucket, Key=key)

    def delete_object(self, bucket: str, key: str) -> dict:
        """Delete an object from a bucket."""
        return self.s3.delete_object(Bucket=bucket, Key=key)

    def list_objects(
        self, bucket: str, prefix: str = "", max_keys: int = 1000
    ) -> dict:
        """List objects in a bucket with an optional prefix."""
        kwargs = {"Bucket": bucket, "MaxKeys": max_keys}
        if prefix:
            kwargs["Prefix"] = prefix
        return self.s3.list_objects_v2(**kwargs)

    def head_object(self, bucket: str, key: str) -> dict:
        """Retrieve object metadata without downloading."""
        return self.s3.head_object(Bucket=bucket, Key=key)

    def copy_object(
        self, src_bucket: str, src_key: str, dst_bucket: str, dst_key: str
    ) -> dict:
        """Copy an object from one location to another."""
        return self.s3.copy_object(
            Bucket=dst_bucket,
            Key=dst_key,
            CopySource={"Bucket": src_bucket, "Key": src_key},
        )

    # ---- Bucket Operations ----

    def create_bucket(self, bucket: str) -> dict:
        """Create a new bucket."""
        return self.s3.create_bucket(Bucket=bucket)

    def delete_bucket(self, bucket: str) -> dict:
        """Delete an empty bucket."""
        return self.s3.delete_bucket(Bucket=bucket)

    def list_buckets(self) -> dict:
        """List all buckets."""
        return self.s3.list_buckets()

    # ---- Multipart Upload ----

    def create_multipart_upload(self, bucket: str, key: str) -> dict:
        """Initiate a multipart upload."""
        return self.s3.create_multipart_upload(Bucket=bucket, Key=key)

    def upload_part(
        self,
        bucket: str,
        key: str,
        upload_id: str,
        part_number: int,
        body: BinaryIO,
    ) -> dict:
        """Upload a part in a multipart upload."""
        return self.s3.upload_part(
            Bucket=bucket,
            Key=key,
            UploadId=upload_id,
            PartNumber=part_number,
            Body=body,
        )

    def complete_multipart_upload(
        self, bucket: str, key: str, upload_id: str, parts: List[dict]
    ) -> dict:
        """Complete a multipart upload."""
        return self.s3.complete_multipart_upload(
            Bucket=bucket,
            Key=key,
            UploadId=upload_id,
            MultipartUpload={"Parts": parts},
        )

    def abort_multipart_upload(
        self, bucket: str, key: str, upload_id: str
    ) -> dict:
        """Abort a multipart upload."""
        return self.s3.abort_multipart_upload(
            Bucket=bucket, Key=key, UploadId=upload_id
        )

    # ---- Nexus Extensions ----

    def vector_search(self, query: str, top_k: int = 20) -> List[dict]:
        """Perform a vector similarity search across indexed buckets.

        Args:
            query: The search query string to be embedded.
            top_k: Number of top results to return.

        Returns:
            List of search results with key, bucket, score, and metadata.
        """
        import urllib.request

        url = f"{self.endpoint_url}/_vector_search"
        payload = json.dumps({"query": query, "top_k": top_k}).encode("utf-8")
        req = urllib.request.Request(
            url,
            data=payload,
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        with urllib.request.urlopen(req) as resp:
            data = json.loads(resp.read().decode("utf-8"))
        return data.get("results", [])

    def fts_search(
        self, bucket: str, query: str, top_k: int = 10
    ) -> List[dict]:
        """Perform a full-text search within a bucket.

        Args:
            bucket: The bucket to search in.
            query: The search query string.
            top_k: Number of top results to return.

        Returns:
            List of search results with key, bucket, score, and snippet.
        """
        import urllib.request

        url = f"{self.endpoint_url}/{bucket}/_fts"
        payload = json.dumps({"query": query, "top_k": top_k}).encode("utf-8")
        req = urllib.request.Request(
            url,
            data=payload,
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        with urllib.request.urlopen(req) as resp:
            data = json.loads(resp.read().decode("utf-8"))
        return data.get("results", [])

    def resumable_upload(
        self, bucket: str, key: str, file_path: str, chunk_size: int = 5 * 1024 * 1024
    ) -> dict:
        """Perform a resumable upload for large files.

        Args:
            bucket: Target bucket name.
            key: Object key.
            file_path: Local file path to upload.
            chunk_size: Size of each chunk in bytes (default 5MB).

        Returns:
            Complete multipart upload response.
        """
        mpu = self.create_multipart_upload(bucket, key)
        upload_id = mpu["UploadId"]
        parts = []

        try:
            with open(file_path, "rb") as f:
                part_number = 1
                while True:
                    chunk = f.read(chunk_size)
                    if not chunk:
                        break
                    from io import BytesIO

                    resp = self.upload_part(
                        bucket,
                        key,
                        upload_id,
                        part_number,
                        BytesIO(chunk),
                    )
                    parts.append(
                        {
                            "PartNumber": part_number,
                            "ETag": resp["ETag"],
                        }
                    )
                    part_number += 1

            return self.complete_multipart_upload(bucket, key, upload_id, parts)
        except Exception:
            self.abort_multipart_upload(bucket, key, upload_id)
            raise
