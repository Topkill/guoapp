import 'package:duanju_app/app_build.dart';
import 'package:duanju_app/local_store.dart';
import 'package:duanju_app/source_gate_dialog.dart';
import 'package:duanju_app/source_gate_taps.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:shared_preferences/shared_preferences.dart';

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();
  // 让输入框光标不再无限闪烁，避免 pumpAndSettle 无法收敛。
  EditableText.debugDeterministicCursor = true;

  Future<LocalStore> create([Map<String, Object> initial = const {}]) async {
    SharedPreferences.setMockInitialValues(Map.of(initial));
    final store = LocalStore(await SharedPreferences.getInstance());
    addTearDown(store.dispose);
    return store;
  }

  test('default visibility hides restricted sources until unlocked', () async {
    final store = await create();
    expect(store.sourceGateEnabled, isFalse);
    expect(store.sourcesUnlocked, isTrue);
    expect(
      store.sources.map((site) => site.id),
      allSourcesEnabled ? ['hongguo', 'sorani'] : ['hongguo'],
    );
    expect(store.allowsSource('sorani'), allSourcesEnabled);
    expect(store.allowsSource('huangdou'), allSourcesEnabled);
  });

  test('enabling the gate hides restricted sources and unlocks once', () async {
    final store = await create();
    await store.enableSourceGate('666');
    expect(store.sourceGateEnabled, isTrue);
    expect(store.sourcesUnlocked, isTrue);
    expect(store.sources.length, allSourcesEnabled ? 9 : 1);

    store.lockSources();
    expect(store.sourcesUnlocked, isFalse);
    expect(store.sources.length, allSourcesEnabled ? 2 : 1);
    expect(store.allowsSource('huangdou'), isFalse);
    expect(store.allowsSource('hongguo'), isTrue);
    if (allSourcesEnabled) expect(store.allowsSource('sorani'), isTrue);

    await expectLater(store.unlockSources('777'), throwsStateError);
    expect(store.sourcesUnlocked, isFalse);
    await store.unlockSources('666');
    expect(store.sourcesUnlocked, isTrue);
    expect(store.sources.length, allSourcesEnabled ? 9 : 1);
  });

  test('a saved gate hides sources again after restart', () async {
    final store = await create();
    await store.enableSourceGate('666');
    store.lockSources();
    final restarted = LocalStore(store.preferences);
    addTearDown(restarted.dispose);
    expect(restarted.sourceGateEnabled, isTrue);
    expect(restarted.sourcesUnlocked, isFalse);
    expect(restarted.sources.length, allSourcesEnabled ? 2 : 1);
    await restarted.unlockSources('666');
    expect(restarted.sourcesUnlocked, isTrue);
  });

  test('disabling the gate restores every compiled source', () async {
    final store = await create();
    await store.enableSourceGate('666');
    store.lockSources();
    await store.disableSourceGate();
    expect(store.sourceGateEnabled, isFalse);
    expect(store.sourcesUnlocked, isTrue);
    expect(store.sources.length, allSourcesEnabled ? 9 : 1);
  });

  test('an invalid pin must be 3 to 12 digits', () async {
    final store = await create();
    await expectLater(store.enableSourceGate('12'), throwsStateError);
    await expectLater(store.enableSourceGate('abcdef'), throwsStateError);
    await expectLater(
      store.enableSourceGate('1234567890123'),
      throwsStateError,
    );
    expect(store.sourceGateEnabled, isFalse);
  });

  test('repeated taps on one entry only fire after six in a row', () {
    final gate = RepeatTapGate();
    for (var i = 0; i < 5; i++) {
      expect(gate.register(2), isFalse);
    }
    expect(gate.register(2), isTrue);
    expect(gate.register(2), isFalse);
    gate.reset();
    expect(gate.register(2), isFalse);
    // 切换入口会清零计数，避免误触发。
    for (var i = 0; i < 5; i++) {
      expect(gate.register(2), isFalse);
    }
    expect(gate.register(0), isFalse);
    expect(gate.register(2), isFalse);
  });

  test('a damaged gate record falls back to visible sources', () async {
    final store = await create({
      'sourceGateEnabled': true,
      'sourceGateSalt': 'bad',
      'sourceGateHash': 'bad',
    });
    expect(store.configurationError, isNull);
    expect(store.sourceGateEnabled, isFalse);
    expect(store.sourcesUnlocked, isTrue);
  });

  testWidgets('the gate dialog offers enabling, unlocking and locking', (
    tester,
  ) async {
    final store = await create();
    await tester.pumpWidget(
      MaterialApp(
        home: Scaffold(
          body: Builder(
            builder: (context) => TextButton(
              onPressed: () => showSourceGateDialog(context, store),
              child: const Text('打开'),
            ),
          ),
        ),
      ),
    );

    // 未启用时给用户「启用密码锁」的选择。
    await tester.tap(find.text('打开'));
    await tester.pumpAndSettle();
    expect(find.text('站源密码锁'), findsOneWidget);
    expect(find.text('启用密码锁'), findsOneWidget);
    expect(find.text('解锁'), findsNothing);

    await tester.enterText(find.byType(TextField).first, '666');
    await tester.enterText(find.byType(TextField).last, '666');
    await tester.tap(find.text('启用密码锁'));
    await tester.pumpAndSettle();
    expect(store.sourceGateEnabled, isTrue);
    expect(store.sourcesUnlocked, isTrue);

    // 已启用且已解锁时提供「重新锁定」与「关闭密码功能」。
    await tester.tap(find.text('打开'));
    await tester.pumpAndSettle();
    expect(find.text('重新锁定'), findsOneWidget);
    expect(find.text('关闭密码功能'), findsOneWidget);
    await tester.tap(find.text('重新锁定'));
    await tester.pumpAndSettle();
    expect(store.sourcesUnlocked, isFalse);

    // 锁定后要求输入密码才能解锁。
    await tester.tap(find.text('打开'));
    await tester.pumpAndSettle();
    expect(find.text('解锁'), findsOneWidget);
    await tester.enterText(find.byType(TextField).first, '666');
    await tester.tap(find.text('解锁'));
    await tester.pumpAndSettle();
    expect(store.sourcesUnlocked, isTrue);

    // 仍可关闭密码功能，恢复全部站源可见。
    await tester.tap(find.text('打开'));
    await tester.pumpAndSettle();
    await tester.tap(find.text('关闭密码功能'));
    await tester.pumpAndSettle();
    expect(store.sourceGateEnabled, isFalse);
    expect(store.sourcesUnlocked, isTrue);
  });
}
