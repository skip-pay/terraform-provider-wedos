# A record set is imported as zone/name/type, with @ for the zone apex.
terraform import wedos_dns_record_set.www example.cz/www/A
terraform import wedos_dns_record_set.mx example.cz/@/MX
