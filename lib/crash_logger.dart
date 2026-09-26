import 'dart:async';
import 'dart:io';
import 'dart:ui';

import 'package:flutter/foundation.dart';
import 'package:path_provider/path_provider.dart';

/// 全局崩溃 / 异常落盘。
///
/// 真机上「播放中数分钟后闪退」这类问题，没有连接 adb 时只能靠日志定位。
/// 这里把未捕获的 Flutter 异常、Zone 异常和底层 PlatformDispatcher 错误追加
/// 写入应用文档目录的 `crash.log`，可通过「文件」/「文档」目录取回。
///
/// 落盘是尽力而为：任何写入失败都静默忽略，绝不在错误处理路径里再抛异常。
void installCrashLogger() {
  FlutterError.onError = (FlutterErrorDetails details) {
    FlutterError.presentError(details);
    unawaited(_writeCrashLog('FlutterError', details.exceptionAsString(),
        details.stack?.toString()));
  };
  PlatformDispatcher.instance.onError = (Object error, StackTrace stack) {
    unawaited(_writeCrashLog('PlatformDispatcher', error.toString(),
        stack.toString()));
    return true;
  };
}

Future<void> _writeCrashLog(
    String kind, String message, String? stack) async {
  try {
    final directory = await getApplicationDocumentsDirectory();
    final file = File('${directory.path}/crash.log');
    final stamp = DateTime.now().toIso8601String();
    final buffer = StringBuffer()
      ..writeln('=== $stamp ===')
      ..writeln('[$kind] $message');
    if (stack != null && stack.isNotEmpty) {
      buffer.writeln(stack);
    }
    buffer.writeln();
    await file.writeAsString(buffer.toString(), mode: FileMode.append);
  } catch (_) {
    // 落盘失败不影响主流程。
  }
}
