output "public_ip" {
  description = "Public IP of the EC2 instance"
  value       = aws_instance.app_server.public_ip
}

output "api_endpoint" {
  description = "API Endpoint"
  value       = "http://${aws_instance.app_server.public_ip}:8080/healthz"
}