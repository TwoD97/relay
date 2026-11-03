import type { TerminalImage } from "./api";

export function validateTerminalImage(blob: Blob) {
  if (!["image/png", "image/jpeg"].includes(blob.type)) throw new Error("Choose a PNG or JPEG image.");
  if (!blob.size || blob.size > 10 * 1024 * 1024) throw new Error("Images must be between 1 byte and 10 MiB.");
}

export function imagePastePath(image: TerminalImage, blob: Blob) {
  if (!image || typeof image.path !== "string" || image.path.length > 4096 || /\p{Cc}/u.test(image.path) || !/^(\/|[A-Za-z]:[\\/])/.test(image.path) || image.mime !== blob.type || image.bytes !== blob.size || !/^[a-f0-9]{64}$/.test(image.sha256)) {
    throw new Error("Relay could not verify the uploaded image path.");
  }
  // A path is input, never a command. Quote shell metacharacters and spaces,
  // preserve it for the provider's image reader, and never append Enter.
  const windows = /^[A-Za-z]:[\\/]/.test(image.path);
  const path = windows ? image.path.replace(/\\/g, "/").replace(/'/g, "''") : image.path.replace(/'/g, "'\\''");
  return ` '${path}' `;
}
