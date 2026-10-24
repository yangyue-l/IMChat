-- MySQL 初始化：仅做基础环境设定，业务表由 GORM AutoMigrate 创建
SET GLOBAL time_zone = '+08:00';
SET NAMES utf8mb4;

CREATE DATABASE IF NOT EXISTS imchat DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
