variable "account_id" {
  description = "The Cloudflare account that owns the tunnel and the Access application."
  type        = string
}

variable "zone_id" {
  description = "The Cloudflare zone the tunnel's hostname is created in. You must already own a domain on Cloudflare: the tunnel is addressed by hostname, and that is this route's one hard prerequisite."
  type        = string
}

variable "hostname" {
  description = "The fully qualified hostname CI connects to, inside the zone above."
  type        = string

  validation {
    condition     = can(regex("^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$", var.hostname))
    error_message = "hostname must be a fully qualified DNS name."
  }
}

variable "name" {
  description = "Name prefix for the tunnel, the Access application and its service token."
  type        = string
  default     = "billet-converge"
}

variable "session_duration" {
  description = "How long an Access session lasts. Short by default: this application fronts SSH to a host that runs CI for other people's code."
  type        = string
  default     = "30m"
}
