// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2019 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package secboot_test

import (
	"crypto"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"path/filepath"
	"strings"

	. "github.com/snapcore/secboot"
	"github.com/snapcore/secboot/internal/testutil"
	snapd_testutil "github.com/snapcore/snapd/testutil"

	. "gopkg.in/check.v1"
)

type cryptTestBase struct {
	testutil.KeyringTestBase

	dir string

	passwordFile string // a newline delimited list of passwords for the mock systemd-ask-password to return

	mockKeyslotsDir   string
	mockKeyslotsCount int

	cryptsetupInvocationCountDir string
	cryptsetupKey                string // The file in which the mock cryptsetup dumps the provided key
	cryptsetupNewkey             string // The file in which the mock cryptsetup dumps the provided new key

	mockSdAskPassword *snapd_testutil.MockCmd
	mockSdCryptsetup  *snapd_testutil.MockCmd
	mockCryptsetup    *snapd_testutil.MockCmd
}

func (ctb *cryptTestBase) SetUpTest(c *C) {
	ctb.KeyringTestBase.SetUpTest(c)

	ctb.dir = c.MkDir()
	ctb.AddCleanup(MockRunDir(ctb.dir))

	ctb.passwordFile = filepath.Join(ctb.dir, "password") // passwords to be returned by the mock sd-ask-password

	ctb.mockKeyslotsCount = 0
	ctb.mockKeyslotsDir = c.MkDir()

	ctb.cryptsetupKey = filepath.Join(ctb.dir, "cryptsetupkey")       // File in which the mock cryptsetup records the passed in key
	ctb.cryptsetupNewkey = filepath.Join(ctb.dir, "cryptsetupnewkey") // File in which the mock cryptsetup records the passed in new key
	ctb.cryptsetupInvocationCountDir = c.MkDir()

	sdAskPasswordBottom := `
head -1 %[1]s
sed -i -e '1,1d' %[1]s
`
	ctb.mockSdAskPassword = snapd_testutil.MockCommand(c, "systemd-ask-password", fmt.Sprintf(sdAskPasswordBottom, ctb.passwordFile))
	ctb.AddCleanup(ctb.mockSdAskPassword.Restore)

	sdCryptsetupBottom := `
key=$(xxd -p < "$4")
for f in "%[1]s"/*; do
    if [ "$key" == "$(xxd -p < "$f")" ]; then
	exit 0
    fi
done


# use a specific error code to differentiate from arbitrary exit 1 elsewhere
exit 5
`
	ctb.mockSdCryptsetup = snapd_testutil.MockCommand(c, c.MkDir()+"/systemd-cryptsetup", fmt.Sprintf(sdCryptsetupBottom, ctb.mockKeyslotsDir))
	ctb.AddCleanup(ctb.mockSdCryptsetup.Restore)
	ctb.AddCleanup(MockSystemdCryptsetupPath(ctb.mockSdCryptsetup.Exe()))

	cryptsetupBottom := `
keyfile=""
action=""

while [ $# -gt 0 ]; do
    case "$1" in
        --key-file)
            keyfile=$2
            shift 2
            ;;
        --type | --cipher | --key-size | --pbkdf | --pbkdf-force-iterations | --pbkdf-memory | --label | --priority | --key-slot | --iter-time)
            shift 2
            ;;
        -*)
            shift
            ;;
        *)
            if [ -z "$action" ]; then
                action=$1
                shift
            else
                break
            fi
    esac
done

new_keyfile=""
if [ "$action" = "luksAddKey" ]; then
    new_keyfile=$2
fi

invocation=$(find %[4]s | wc -l)
mktemp %[4]s/XXXX

dump_key()
{
    in=$1
    out=$2

    if [ -z "$in" ]; then
	touch "$out"
    elif [ "$in" == "-" ]; then
	cat /dev/stdin > "$out"
    else
	cat "$in" > "$out"
    fi
}

dump_key "$keyfile" "%[2]s.$invocation"
dump_key "$new_keyfile" "%[3]s.$invocation"
`

	ctb.mockCryptsetup = snapd_testutil.MockCommand(c, "cryptsetup", fmt.Sprintf(cryptsetupBottom, ctb.dir, ctb.cryptsetupKey, ctb.cryptsetupNewkey, ctb.cryptsetupInvocationCountDir))
	ctb.AddCleanup(ctb.mockCryptsetup.Restore)
}

func (ctb *cryptTestBase) addMockKeyslot(c *C, key []byte) {
	c.Assert(ioutil.WriteFile(filepath.Join(ctb.mockKeyslotsDir, fmt.Sprintf("%d", ctb.mockKeyslotsCount)), key, 0644), IsNil)
	ctb.mockKeyslotsCount++
}

func (ctb *cryptTestBase) newPrimaryKey() []byte {
	key := make([]byte, 32)
	rand.Read(key)
	return key
}

func (ctb *cryptTestBase) newRecoveryKey() RecoveryKey {
	var key RecoveryKey
	rand.Read(key[:])
	return key
}

func (ctb *cryptTestBase) checkRecoveryKeyInKeyring(c *C, prefix, path string, expected RecoveryKey) {
	// The following test will fail if the user keyring isn't reachable from the session keyring. If the test have succeeded
	// so far, mark the current test as expected to fail.
	if !ctb.ProcessPossessesUserKeyringKeys && !c.Failed() {
		c.ExpectFailure("Cannot possess user keys because the user keyring isn't reachable from the session keyring")
	}

	key, err := GetDiskUnlockKeyFromKernel(prefix, path, false)
	c.Check(err, IsNil)
	c.Check(key, DeepEquals, DiskUnlockKey(expected[:]))
}

func (ctb *cryptTestBase) addTryPassphrases(c *C, passphrases []string) {
	for _, passphrase := range passphrases {
		f, err := os.OpenFile(ctb.passwordFile, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0644)
		c.Assert(err, IsNil)
		_, err = f.WriteString(passphrase + "\n")
		c.Check(err, IsNil)
		f.Close()
	}
}

type cryptSuite struct {
	cryptTestBase
	keyDataTestBase
	snapModelTestBase
}

var _ = Suite(&cryptSuite{})

func (s *cryptSuite) SetUpSuite(c *C) {
	s.cryptTestBase.SetUpSuite(c)
	s.keyDataTestBase.SetUpSuite(c)
}

func (s *cryptSuite) SetUpTest(c *C) {
	s.cryptTestBase.SetUpTest(c)
	s.keyDataTestBase.SetUpTest(c)
}

func (s *cryptSuite) checkKeyDataKeysInKeyring(c *C, prefix, path string, expectedKey DiskUnlockKey, expectedAuxKey AuxiliaryKey) {
	// The following test will fail if the user keyring isn't reachable from the session keyring. If the test have succeeded
	// so far, mark the current test as expected to fail.
	if !s.ProcessPossessesUserKeyringKeys && !c.Failed() {
		c.ExpectFailure("Cannot possess user keys because the user keyring isn't reachable from the session keyring")
	}

	key, err := GetDiskUnlockKeyFromKernel(prefix, path, false)
	c.Check(err, IsNil)
	c.Check(key, DeepEquals, expectedKey)

	auxKey, err := GetAuxiliaryKeyFromKernel(prefix, path, false)
	c.Check(err, IsNil)
	c.Check(auxKey, DeepEquals, expectedAuxKey)
}

func (s *cryptSuite) newMultipleNamedKeyData(c *C, ids []*KeyID) (keyData []*KeyData, keys []DiskUnlockKey, auxKeys []AuxiliaryKey) {
	for _, id := range ids {
		key, auxKey := s.newKeyDataKeys(c, 32, 32)
		protected := s.mockProtectKeys(c, key, auxKey, crypto.SHA256)

		kd, err := NewKeyData(protected)
		c.Assert(err, IsNil)

		w := makeMockKeyDataWriter()
		c.Check(kd.WriteAtomic(w), IsNil)

		r := &mockKeyDataReader{*id, w.Reader()}
		kd, err = ReadKeyData(r)
		c.Assert(err, IsNil)

		keyData = append(keyData, kd)
		keys = append(keys, key)
		auxKeys = append(auxKeys, auxKey)
	}

	return keyData, keys, auxKeys
}

func (s *cryptSuite) newNamedKeyData(c *C, id *KeyID) (*KeyData, DiskUnlockKey, AuxiliaryKey) {
	keyData, keys, auxKeys := s.newMultipleNamedKeyData(c, []*KeyID{id})
	return keyData[0], keys[0], auxKeys[0]
}

type testActivateVolumeWithRecoveryKeyData struct {
	recoveryKey         RecoveryKey
	volumeName          string
	sourceDevicePath    string
	tries               int
	activateOptions     []string
	keyringPrefix       string
	recoveryPassphrases []string
	sdCryptsetupCalls   int
}

func (s *cryptSuite) testActivateVolumeWithRecoveryKey(c *C, data *testActivateVolumeWithRecoveryKeyData) {
	s.addMockKeyslot(c, data.recoveryKey[:])

	c.Assert(ioutil.WriteFile(s.passwordFile, []byte(strings.Join(data.recoveryPassphrases, "\n")+"\n"), 0644), IsNil)

	options := ActivateVolumeOptions{RecoveryKeyTries: data.tries, ActivateOptions: data.activateOptions, KeyringPrefix: data.keyringPrefix}
	c.Assert(ActivateVolumeWithRecoveryKey(data.volumeName, data.sourceDevicePath, nil, &options), IsNil)

	c.Check(len(s.mockSdAskPassword.Calls()), Equals, len(data.recoveryPassphrases))
	for _, call := range s.mockSdAskPassword.Calls() {
		c.Check(call, DeepEquals, []string{"systemd-ask-password", "--icon", "drive-harddisk", "--id",
			filepath.Base(os.Args[0]) + ":" + data.sourceDevicePath, "Please enter the recovery key for disk " + data.sourceDevicePath + ":"})
	}

	c.Check(len(s.mockSdCryptsetup.Calls()), Equals, data.sdCryptsetupCalls)
	for _, call := range s.mockSdCryptsetup.Calls() {
		c.Assert(len(call), Equals, 6)
		c.Check(call[0:4], DeepEquals, []string{"systemd-cryptsetup", "attach", data.volumeName, data.sourceDevicePath})
		c.Check(call[4], Matches, filepath.Join(s.dir, filepath.Base(os.Args[0]))+"\\.[0-9]+/fifo")
		c.Check(call[5], Equals, strings.Join(append(data.activateOptions, "tries=1"), ","))
	}

	// This should be done last because it may fail in some circumstances.
	s.checkRecoveryKeyInKeyring(c, data.keyringPrefix, data.sourceDevicePath, data.recoveryKey)
}

func (s *cryptSuite) TestActivateVolumeWithRecoveryKey1(c *C) {
	// Test with a recovery key which is entered with a hyphen between each group of 5 digits.
	recoveryKey := s.newRecoveryKey()
	s.testActivateVolumeWithRecoveryKey(c, &testActivateVolumeWithRecoveryKeyData{
		recoveryKey:         recoveryKey,
		volumeName:          "data",
		sourceDevicePath:    "/dev/sda1",
		tries:               1,
		recoveryPassphrases: []string{recoveryKey.String()},
		sdCryptsetupCalls:   1,
	})
}

func (s *cryptSuite) TestActivateVolumeWithRecoveryKey2(c *C) {
	// Test with a recovery key which is entered without a hyphen between each group of 5 digits.
	recoveryKey := s.newRecoveryKey()
	s.testActivateVolumeWithRecoveryKey(c, &testActivateVolumeWithRecoveryKeyData{
		recoveryKey:         recoveryKey,
		volumeName:          "data",
		sourceDevicePath:    "/dev/sda1",
		tries:               1,
		recoveryPassphrases: []string{strings.Replace(recoveryKey.String(), "-", "", -1)},
		sdCryptsetupCalls:   1,
	})
}

func (s *cryptSuite) TestActivateVolumeWithRecoveryKey3(c *C) {
	// Test that activation succeeds when the correct recovery key is provided on the second attempt.
	recoveryKey := s.newRecoveryKey()
	s.testActivateVolumeWithRecoveryKey(c, &testActivateVolumeWithRecoveryKeyData{
		recoveryKey:      recoveryKey,
		volumeName:       "data",
		sourceDevicePath: "/dev/sda1",
		tries:            2,
		recoveryPassphrases: []string{
			"00000-00000-00000-00000-00000-00000-00000-00000",
			recoveryKey.String(),
		},
		sdCryptsetupCalls: 2,
	})
}

func (s *cryptSuite) TestActivateVolumeWithRecoveryKey4(c *C) {
	// Test that activation succeeds when the correct recovery key is provided on the second attempt, and the first
	// attempt is badly formatted.
	recoveryKey := s.newRecoveryKey()
	s.testActivateVolumeWithRecoveryKey(c, &testActivateVolumeWithRecoveryKeyData{
		recoveryKey:      recoveryKey,
		volumeName:       "data",
		sourceDevicePath: "/dev/sda1",
		tries:            2,
		recoveryPassphrases: []string{
			"1234",
			recoveryKey.String(),
		},
		sdCryptsetupCalls: 1,
	})
}

func (s *cryptSuite) TestActivateVolumeWithRecoveryKey5(c *C) {
	// Test with additional options passed to systemd-cryptsetup.
	recoveryKey := s.newRecoveryKey()
	s.testActivateVolumeWithRecoveryKey(c, &testActivateVolumeWithRecoveryKeyData{
		recoveryKey:         recoveryKey,
		volumeName:          "data",
		sourceDevicePath:    "/dev/sda1",
		tries:               1,
		activateOptions:     []string{"foo", "bar"},
		recoveryPassphrases: []string{recoveryKey.String()},
		sdCryptsetupCalls:   1,
	})
}

func (s *cryptSuite) TestActivateVolumeWithRecoveryKey6(c *C) {
	// Test with a different volume name / device path.
	recoveryKey := s.newRecoveryKey()
	s.testActivateVolumeWithRecoveryKey(c, &testActivateVolumeWithRecoveryKeyData{
		recoveryKey:         recoveryKey,
		volumeName:          "foo",
		sourceDevicePath:    "/dev/vdb2",
		tries:               1,
		recoveryPassphrases: []string{recoveryKey.String()},
		sdCryptsetupCalls:   1,
	})
}

func (s *cryptSuite) TestActivateVolumeWithRecoveryKey7(c *C) {
	// Test with a different keyring prefix
	recoveryKey := s.newRecoveryKey()
	s.testActivateVolumeWithRecoveryKey(c, &testActivateVolumeWithRecoveryKeyData{
		recoveryKey:         recoveryKey,
		volumeName:          "data",
		sourceDevicePath:    "/dev/sda1",
		tries:               1,
		keyringPrefix:       "test",
		recoveryPassphrases: []string{recoveryKey.String()},
		sdCryptsetupCalls:   1,
	})
}

type testActivateVolumeWithRecoveryKeyUsingKeyReaderData struct {
	recoveryKey             RecoveryKey
	tries                   int
	recoveryKeyFileContents string
	recoveryPassphrases     []string
	sdCryptsetupCalls       int
}

func (s *cryptSuite) testActivateVolumeWithRecoveryKeyUsingKeyReader(c *C, data *testActivateVolumeWithRecoveryKeyUsingKeyReaderData) {
	s.addMockKeyslot(c, data.recoveryKey[:])

	c.Assert(ioutil.WriteFile(s.passwordFile, []byte(strings.Join(data.recoveryPassphrases, "\n")+"\n"), 0644), IsNil)
	c.Assert(ioutil.WriteFile(filepath.Join(s.dir, "keyfile"), []byte(data.recoveryKeyFileContents), 0644), IsNil)

	r, err := os.Open(filepath.Join(s.dir, "keyfile"))
	c.Assert(err, IsNil)
	defer r.Close()

	options := ActivateVolumeOptions{RecoveryKeyTries: data.tries}
	c.Assert(ActivateVolumeWithRecoveryKey("data", "/dev/sda1", r, &options), IsNil)

	c.Check(len(s.mockSdAskPassword.Calls()), Equals, len(data.recoveryPassphrases))
	for _, call := range s.mockSdAskPassword.Calls() {
		c.Check(call, DeepEquals, []string{"systemd-ask-password", "--icon", "drive-harddisk", "--id",
			filepath.Base(os.Args[0]) + ":/dev/sda1", "Please enter the recovery key for disk /dev/sda1:"})
	}

	c.Check(len(s.mockSdCryptsetup.Calls()), Equals, data.sdCryptsetupCalls)
	for _, call := range s.mockSdCryptsetup.Calls() {
		c.Assert(len(call), Equals, 6)
		c.Check(call[0:4], DeepEquals, []string{"systemd-cryptsetup", "attach", "data", "/dev/sda1"})
		c.Check(call[4], Matches, filepath.Join(s.dir, filepath.Base(os.Args[0]))+"\\.[0-9]+/fifo")
		c.Check(call[5], Equals, "tries=1")
	}

	// This should be done last because it may fail in some circumstances.
	s.checkRecoveryKeyInKeyring(c, "", "/dev/sda1", data.recoveryKey)
}

func (s *cryptSuite) TestActivateVolumeWithRecoveryKeyUsingKeyReader1(c *C) {
	// Test with the correct recovery key supplied via a io.Reader, with a hyphen separating each group of 5 digits.
	recoveryKey := s.newRecoveryKey()
	s.testActivateVolumeWithRecoveryKeyUsingKeyReader(c, &testActivateVolumeWithRecoveryKeyUsingKeyReaderData{
		recoveryKey:             recoveryKey,
		tries:                   1,
		recoveryKeyFileContents: recoveryKey.String() + "\n",
		sdCryptsetupCalls:       1,
	})
}

func (s *cryptSuite) TestActivateVolumeWithRecoveryKeyUsingKeyReader2(c *C) {
	// Test with the correct recovery key supplied via a io.Reader, without a hyphen separating each group of 5 digits.
	recoveryKey := s.newRecoveryKey()
	s.testActivateVolumeWithRecoveryKeyUsingKeyReader(c, &testActivateVolumeWithRecoveryKeyUsingKeyReaderData{
		recoveryKey:             recoveryKey,
		tries:                   1,
		recoveryKeyFileContents: strings.Replace(recoveryKey.String(), "-", "", -1) + "\n",
		sdCryptsetupCalls:       1,
	})
}

func (s *cryptSuite) TestActivateVolumeWithRecoveryKeyUsingKeyReader3(c *C) {
	// Test with the correct recovery key supplied via a io.Reader when the key doesn't end in a newline.
	recoveryKey := s.newRecoveryKey()
	s.testActivateVolumeWithRecoveryKeyUsingKeyReader(c, &testActivateVolumeWithRecoveryKeyUsingKeyReaderData{
		recoveryKey:             recoveryKey,
		tries:                   1,
		recoveryKeyFileContents: recoveryKey.String(),
		sdCryptsetupCalls:       1,
	})
}

func (s *cryptSuite) TestActivateVolumeWithRecoveryKeyUsingKeyReader4(c *C) {
	// Test that falling back to requesting a recovery key works if the one provided by the io.Reader is incorrect.
	recoveryKey := s.newRecoveryKey()
	s.testActivateVolumeWithRecoveryKeyUsingKeyReader(c, &testActivateVolumeWithRecoveryKeyUsingKeyReaderData{
		recoveryKey:             recoveryKey,
		tries:                   2,
		recoveryKeyFileContents: "00000-00000-00000-00000-00000-00000-00000-00000\n",
		recoveryPassphrases:     []string{recoveryKey.String()},
		sdCryptsetupCalls:       2,
	})
}

func (s *cryptSuite) TestActivateVolumeWithRecoveryKeyUsingKeyReader5(c *C) {
	// Test that falling back to requesting a recovery key works if the one provided by the io.Reader is badly formatted.
	recoveryKey := s.newRecoveryKey()
	s.testActivateVolumeWithRecoveryKeyUsingKeyReader(c, &testActivateVolumeWithRecoveryKeyUsingKeyReaderData{
		recoveryKey:             recoveryKey,
		tries:                   2,
		recoveryKeyFileContents: "5678\n",
		recoveryPassphrases:     []string{recoveryKey.String()},
		sdCryptsetupCalls:       1,
	})
}

func (s *cryptSuite) TestActivateVolumeWithRecoveryKeyUsingKeyReader6(c *C) {
	// Test that falling back to requesting a recovery key works if the provided io.Reader is backed by an empty buffer,
	// without using up a try.
	recoveryKey := s.newRecoveryKey()
	s.testActivateVolumeWithRecoveryKeyUsingKeyReader(c, &testActivateVolumeWithRecoveryKeyUsingKeyReaderData{
		recoveryKey:         recoveryKey,
		tries:               1,
		recoveryPassphrases: []string{recoveryKey.String()},
		sdCryptsetupCalls:   1,
	})
}

type testParseRecoveryKeyData struct {
	formatted string
	expected  []byte
}

func (s *cryptSuite) testParseRecoveryKey(c *C, data *testParseRecoveryKeyData) {
	k, err := ParseRecoveryKey(data.formatted)
	c.Check(err, IsNil)
	c.Check(k[:], DeepEquals, data.expected)
}

func (s *cryptSuite) TestParseRecoveryKey1(c *C) {
	s.testParseRecoveryKey(c, &testParseRecoveryKeyData{
		formatted: "00000-00000-00000-00000-00000-00000-00000-00000",
		expected:  testutil.DecodeHexString(c, "00000000000000000000000000000000"),
	})
}

func (s *cryptSuite) TestParseRecoveryKey2(c *C) {
	s.testParseRecoveryKey(c, &testParseRecoveryKeyData{
		formatted: "61665-00531-54469-09783-47273-19035-40077-28287",
		expected:  testutil.DecodeHexString(c, "e1f01302c5d43726a9b85b4a8d9c7f6e"),
	})
}

func (s *cryptSuite) TestParseRecoveryKey3(c *C) {
	s.testParseRecoveryKey(c, &testParseRecoveryKeyData{
		formatted: "6166500531544690978347273190354007728287",
		expected:  testutil.DecodeHexString(c, "e1f01302c5d43726a9b85b4a8d9c7f6e"),
	})
}

type testParseRecoveryKeyErrorHandlingData struct {
	formatted      string
	errChecker     Checker
	errCheckerArgs []interface{}
}

func (s *cryptSuite) testParseRecoveryKeyErrorHandling(c *C, data *testParseRecoveryKeyErrorHandlingData) {
	_, err := ParseRecoveryKey(data.formatted)
	c.Check(err, data.errChecker, data.errCheckerArgs...)
}

func (s *cryptSuite) TestParseRecoveryKeyErrorHandling1(c *C) {
	s.testParseRecoveryKeyErrorHandling(c, &testParseRecoveryKeyErrorHandlingData{
		formatted:      "00000-1234",
		errChecker:     ErrorMatches,
		errCheckerArgs: []interface{}{"incorrectly formatted: insufficient characters"},
	})
}

func (s *cryptSuite) TestParseRecoveryKeyErrorHandling2(c *C) {
	s.testParseRecoveryKeyErrorHandling(c, &testParseRecoveryKeyErrorHandlingData{
		formatted:      "00000-123bc",
		errChecker:     ErrorMatches,
		errCheckerArgs: []interface{}{"incorrectly formatted: strconv.ParseUint: parsing \"123bc\": invalid syntax"},
	})
}

func (s *cryptSuite) TestParseRecoveryKeyErrorHandling3(c *C) {
	s.testParseRecoveryKeyErrorHandling(c, &testParseRecoveryKeyErrorHandlingData{
		formatted:      "00000-00000-00000-00000-00000-00000-00000-00000-00000",
		errChecker:     ErrorMatches,
		errCheckerArgs: []interface{}{"incorrectly formatted: too many characters"},
	})
}

func (s *cryptSuite) TestParseRecoveryKeyErrorHandling4(c *C) {
	s.testParseRecoveryKeyErrorHandling(c, &testParseRecoveryKeyErrorHandlingData{
		formatted:      "-00000-00000-00000-00000-00000-00000-00000-00000",
		errChecker:     ErrorMatches,
		errCheckerArgs: []interface{}{"incorrectly formatted: strconv.ParseUint: parsing \"-0000\": invalid syntax"},
	})
}

func (s *cryptSuite) TestParseRecoveryKeyErrorHandling5(c *C) {
	s.testParseRecoveryKeyErrorHandling(c, &testParseRecoveryKeyErrorHandlingData{
		formatted:      "00000-00000-00000-00000-00000-00000-00000-00000-",
		errChecker:     ErrorMatches,
		errCheckerArgs: []interface{}{"incorrectly formatted: too many characters"},
	})
}

type testRecoveryKeyStringifyData struct {
	key      []byte
	expected string
}

func (s *cryptSuite) testRecoveryKeyStringify(c *C, data *testRecoveryKeyStringifyData) {
	var key RecoveryKey
	copy(key[:], data.key)
	c.Check(key.String(), Equals, data.expected)
}

func (s *cryptSuite) TestRecoveryKeyStringify1(c *C) {
	s.testRecoveryKeyStringify(c, &testRecoveryKeyStringifyData{
		expected: "00000-00000-00000-00000-00000-00000-00000-00000",
	})
}

func (s *cryptSuite) TestRecoveryKeyStringify2(c *C) {
	s.testRecoveryKeyStringify(c, &testRecoveryKeyStringifyData{
		key:      testutil.DecodeHexString(c, "e1f01302c5d43726a9b85b4a8d9c7f6e"),
		expected: "61665-00531-54469-09783-47273-19035-40077-28287",
	})
}

type testActivateVolumeWithRecoveryKeyErrorHandlingData struct {
	tries               int
	activateOptions     []string
	recoveryPassphrases []string
	sdCryptsetupCalls   int
	errChecker          Checker
	errCheckerArgs      []interface{}
}

func (s *cryptSuite) testActivateVolumeWithRecoveryKeyErrorHandling(c *C, data *testActivateVolumeWithRecoveryKeyErrorHandlingData) {
	recoveryKey := s.newRecoveryKey()
	s.addMockKeyslot(c, recoveryKey[:])

	c.Assert(ioutil.WriteFile(s.passwordFile, []byte(strings.Join(data.recoveryPassphrases, "\n")+"\n"), 0644), IsNil)

	options := ActivateVolumeOptions{RecoveryKeyTries: data.tries, ActivateOptions: data.activateOptions}
	c.Check(ActivateVolumeWithRecoveryKey("data", "/dev/sda1", nil, &options), data.errChecker, data.errCheckerArgs...)

	c.Check(len(s.mockSdAskPassword.Calls()), Equals, len(data.recoveryPassphrases))
	for _, call := range s.mockSdAskPassword.Calls() {
		c.Check(call, DeepEquals, []string{"systemd-ask-password", "--icon", "drive-harddisk", "--id",
			filepath.Base(os.Args[0]) + ":/dev/sda1", "Please enter the recovery key for disk /dev/sda1:"})
	}

	c.Check(len(s.mockSdCryptsetup.Calls()), Equals, data.sdCryptsetupCalls)
	for _, call := range s.mockSdCryptsetup.Calls() {
		c.Assert(len(call), Equals, 6)
		c.Check(call[0:4], DeepEquals, []string{"systemd-cryptsetup", "attach", "data", "/dev/sda1"})
		c.Check(call[4], Matches, filepath.Join(s.dir, filepath.Base(os.Args[0]))+"\\.[0-9]+/fifo")
		c.Check(call[5], Equals, "tries=1")
		c.Check(call[5], Equals, strings.Join(append(data.activateOptions, "tries=1"), ","))
	}
}

func (s *cryptSuite) TestActivateVolumeWithRecoveryKeyErrorHandling1(c *C) {
	// Test with an invalid RecoveryKeyTries value.
	s.testActivateVolumeWithRecoveryKeyErrorHandling(c, &testActivateVolumeWithRecoveryKeyErrorHandlingData{
		tries:          -1,
		errChecker:     ErrorMatches,
		errCheckerArgs: []interface{}{"invalid RecoveryKeyTries"},
	})
}

func (s *cryptSuite) TestActivateVolumeWithRecoveryKeyErrorHandling2(c *C) {
	// Test with Tries set to zero.
	s.testActivateVolumeWithRecoveryKeyErrorHandling(c, &testActivateVolumeWithRecoveryKeyErrorHandlingData{
		tries:          0,
		errChecker:     ErrorMatches,
		errCheckerArgs: []interface{}{"no recovery key tries permitted"},
	})
}

func (s *cryptSuite) TestActivateVolumeWithRecoveryKeyErrorHandling3(c *C) {
	// Test that adding "tries=" to ActivateOptions fails.
	s.testActivateVolumeWithRecoveryKeyErrorHandling(c, &testActivateVolumeWithRecoveryKeyErrorHandlingData{
		tries:           1,
		activateOptions: []string{"tries=2"},
		errChecker:      ErrorMatches,
		errCheckerArgs:  []interface{}{"cannot specify the \"tries=\" option for systemd-cryptsetup"},
	})
}

func (s *cryptSuite) TestActivateVolumeWithRecoveryKeyErrorHandling4(c *C) {
	// Test with a badly formatted recovery key.
	s.testActivateVolumeWithRecoveryKeyErrorHandling(c, &testActivateVolumeWithRecoveryKeyErrorHandlingData{
		tries:               1,
		recoveryPassphrases: []string{"00000-1234"},
		errChecker:          ErrorMatches,
		errCheckerArgs:      []interface{}{"cannot decode recovery key: incorrectly formatted: insufficient characters"},
	})
}

func (s *cryptSuite) TestActivateVolumeWithRecoveryKeyErrorHandling5(c *C) {
	// Test with a badly formatted recovery key.
	s.testActivateVolumeWithRecoveryKeyErrorHandling(c, &testActivateVolumeWithRecoveryKeyErrorHandlingData{
		tries:               1,
		recoveryPassphrases: []string{"00000-123bc"},
		errChecker:          ErrorMatches,
		errCheckerArgs:      []interface{}{"cannot decode recovery key: incorrectly formatted: strconv.ParseUint: parsing \"123bc\": invalid syntax"},
	})
}

func (s *cryptSuite) TestActivateVolumeWithRecoveryKeyErrorHandling6(c *C) {
	// Test with the wrong recovery key.
	s.testActivateVolumeWithRecoveryKeyErrorHandling(c, &testActivateVolumeWithRecoveryKeyErrorHandlingData{
		tries:               1,
		recoveryPassphrases: []string{"00000-00000-00000-00000-00000-00000-00000-00000"},
		sdCryptsetupCalls:   1,
		errChecker:          ErrorMatches,
		errCheckerArgs:      []interface{}{"cannot activate volume: " + s.mockSdCryptsetup.Exe() + " failed: exit status 5"},
	})
}

func (s *cryptSuite) TestActivateVolumeWithRecoveryKeyErrorHandling7(c *C) {
	// Test that the last error is returned when there are consecutive failures for different reasons.
	s.testActivateVolumeWithRecoveryKeyErrorHandling(c, &testActivateVolumeWithRecoveryKeyErrorHandlingData{
		tries:               2,
		recoveryPassphrases: []string{"00000-00000-00000-00000-00000-00000-00000-00000", "1234"},
		sdCryptsetupCalls:   1,
		errChecker:          ErrorMatches,
		errCheckerArgs:      []interface{}{"cannot decode recovery key: incorrectly formatted: insufficient characters"},
	})
}

func (s *cryptSuite) TestActivateVolumeWithRecoveryKeyErrorHandling8(c *C) {
	// Test with a badly formatted recovery key.
	s.testActivateVolumeWithRecoveryKeyErrorHandling(c, &testActivateVolumeWithRecoveryKeyErrorHandlingData{
		tries:               1,
		recoveryPassphrases: []string{"00000-00000-00000-00000-00000-00000-00000-00000-00000"},
		errChecker:          ErrorMatches,
		errCheckerArgs:      []interface{}{"cannot decode recovery key: incorrectly formatted: too many characters"},
	})
}

type testActivateVolumeWithKeyDataData struct {
	authorizedModels []SnapModel
	volumeName       string
	sourceDevicePath string
	keyringPrefix    string
	model            SnapModel
	authorized       bool
}

func (s *cryptSuite) testActivateVolumeWithKeyData(c *C, data *testActivateVolumeWithKeyDataData) {
	keyData, key, auxKey := s.newNamedKeyData(c, &KeyID{})
	s.addMockKeyslot(c, key)

	c.Check(keyData.SetAuthorizedSnapModels(auxKey, data.authorizedModels...), IsNil)

	options := &ActivateVolumeOptions{KeyringPrefix: data.keyringPrefix}
	modelChecker, err := ActivateVolumeWithKeyData(data.volumeName, data.sourceDevicePath, keyData, options)
	c.Assert(err, IsNil)

	authorized, err := modelChecker.IsModelAuthorized(data.model)
	c.Check(err, IsNil)
	c.Check(authorized, Equals, data.authorized)

	c.Check(s.mockSdAskPassword.Calls(), HasLen, 0)
	c.Assert(s.mockSdCryptsetup.Calls(), HasLen, 1)
	c.Assert(s.mockSdCryptsetup.Calls()[0], HasLen, 6)

	c.Check(s.mockSdCryptsetup.Calls()[0][0:4], DeepEquals, []string{"systemd-cryptsetup", "attach", data.volumeName, data.sourceDevicePath})
	c.Check(s.mockSdCryptsetup.Calls()[0][4], Matches, filepath.Join(s.dir, filepath.Base(os.Args[0]))+"\\.[0-9]+/fifo")
	c.Check(s.mockSdCryptsetup.Calls()[0][5], Equals, "tries=1")

	// This should be done last because it may fail in some circumstances.
	s.checkKeyDataKeysInKeyring(c, data.keyringPrefix, data.sourceDevicePath, key, auxKey)
}

func (s *cryptSuite) TestActivateVolumeWithKeyData1(c *C) {
	models := []SnapModel{
		s.makeMockCore20ModelAssertion(c, map[string]interface{}{
			"authority-id": "fake-brand",
			"series":       "16",
			"brand-id":     "fake-brand",
			"model":        "fake-model",
			"grade":        "secured",
		}, "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij")}

	s.testActivateVolumeWithKeyData(c, &testActivateVolumeWithKeyDataData{
		authorizedModels: models,
		volumeName:       "data",
		sourceDevicePath: "/dev/sda1",
		model:            models[0],
		authorized:       true})
}

func (s *cryptSuite) TestActivateVolumeWithKeyData2(c *C) {
	// Test with different volumeName / sourceDevicePath
	models := []SnapModel{
		s.makeMockCore20ModelAssertion(c, map[string]interface{}{
			"authority-id": "fake-brand",
			"series":       "16",
			"brand-id":     "fake-brand",
			"model":        "fake-model",
			"grade":        "secured",
		}, "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij")}

	s.testActivateVolumeWithKeyData(c, &testActivateVolumeWithKeyDataData{
		authorizedModels: models,
		volumeName:       "foo",
		sourceDevicePath: "/dev/vda2",
		model:            models[0],
		authorized:       true})
}

func (s *cryptSuite) TestActivateVolumeWithKeyData3(c *C) {
	// Test with different authorized models
	models := []SnapModel{
		s.makeMockCore20ModelAssertion(c, map[string]interface{}{
			"authority-id": "fake-brand",
			"series":       "16",
			"brand-id":     "fake-brand",
			"model":        "fake-model",
			"grade":        "secured",
		}, "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij"),
		s.makeMockCore20ModelAssertion(c, map[string]interface{}{
			"authority-id": "fake-brand",
			"series":       "16",
			"brand-id":     "fake-brand",
			"model":        "other-model",
			"grade":        "secured",
		}, "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij")}

	s.testActivateVolumeWithKeyData(c, &testActivateVolumeWithKeyDataData{
		authorizedModels: models,
		volumeName:       "data",
		sourceDevicePath: "/dev/sda1",
		model:            models[0],
		authorized:       true})
}

func (s *cryptSuite) TestActivateVolumeWithKeyData4(c *C) {
	// Test with unauthorized model
	models := []SnapModel{
		s.makeMockCore20ModelAssertion(c, map[string]interface{}{
			"authority-id": "fake-brand",
			"series":       "16",
			"brand-id":     "fake-brand",
			"model":        "fake-model",
			"grade":        "secured",
		}, "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij")}

	s.testActivateVolumeWithKeyData(c, &testActivateVolumeWithKeyDataData{
		authorizedModels: models,
		volumeName:       "data",
		sourceDevicePath: "/dev/sda1",
		model: s.makeMockCore20ModelAssertion(c, map[string]interface{}{
			"authority-id": "fake-brand",
			"series":       "16",
			"brand-id":     "fake-brand",
			"model":        "other-model",
			"grade":        "secured",
		}, "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij"),
		authorized: false})
}

type testActivateVolumeWithKeyDataErrorHandlingData struct {
	primaryKey  DiskUnlockKey
	recoveryKey RecoveryKey

	passphrases []string

	recoveryKeyTries int
	keyringPrefix    string

	keyData *KeyData

	errChecker     Checker
	errCheckerArgs []interface{}

	sdCryptsetupCalls int
}

func (s *cryptSuite) testActivateVolumeWithKeyDataErrorHandling(c *C, data *testActivateVolumeWithKeyDataErrorHandlingData) {
	s.addMockKeyslot(c, data.primaryKey)
	s.addMockKeyslot(c, data.recoveryKey[:])

	s.addTryPassphrases(c, data.passphrases)

	options := &ActivateVolumeOptions{
		RecoveryKeyTries: data.recoveryKeyTries,
		KeyringPrefix:    data.keyringPrefix}
	modelChecker, err := ActivateVolumeWithKeyData("data", "/dev/sda1", data.keyData, options)
	c.Check(modelChecker, IsNil)
	if data.errChecker != nil {
		c.Check(err, data.errChecker, data.errCheckerArgs...)
	} else {
		c.Check(err, IsNil)
	}

	c.Check(s.mockSdAskPassword.Calls(), HasLen, len(data.passphrases))
	for _, call := range s.mockSdAskPassword.Calls() {
		c.Check(call, DeepEquals, []string{"systemd-ask-password", "--icon", "drive-harddisk", "--id",
			filepath.Base(os.Args[0]) + ":/dev/sda1", "Please enter the recovery key for disk /dev/sda1:"})
	}

	c.Check(s.mockSdCryptsetup.Calls(), HasLen, data.sdCryptsetupCalls)
	for _, call := range s.mockSdCryptsetup.Calls() {
		c.Assert(call, HasLen, 6)
		c.Check(call[0:4], DeepEquals, []string{"systemd-cryptsetup", "attach", "data", "/dev/sda1"})
		c.Check(call[4], Matches, filepath.Join(s.dir, filepath.Base(os.Args[0]))+"\\.[0-9]+/fifo")
		c.Check(call[5], Equals, "tries=1")
	}

	if data.errChecker != nil {
		return
	}

	// This should be done last because it may fail in some circumstances.
	s.checkRecoveryKeyInKeyring(c, data.keyringPrefix, "/dev/sda1", data.recoveryKey)
}

func (s *cryptSuite) TestActivateVolumeWithKeyDataErrorHandling1(c *C) {
	// Test with an invalid value for RecoveryKeyTries.
	keyData, _, _ := s.newNamedKeyData(c, &KeyID{})

	s.testActivateVolumeWithKeyDataErrorHandling(c, &testActivateVolumeWithKeyDataErrorHandlingData{
		recoveryKeyTries: -1,
		keyData:          keyData,
		errChecker:       ErrorMatches,
		errCheckerArgs:   []interface{}{"invalid RecoveryKeyTries"}})
}

func (s *cryptSuite) TestActivateVolumeWithKeyDataErrorHandling2(c *C) {
	// Test that recovery fallback works with the platform is unavailable
	keyData, key, _ := s.newNamedKeyData(c, &KeyID{})
	recoveryKey := s.newRecoveryKey()

	s.handler.state = mockPlatformDeviceStateUnavailable

	s.testActivateVolumeWithKeyDataErrorHandling(c, &testActivateVolumeWithKeyDataErrorHandlingData{
		primaryKey:        key,
		recoveryKey:       recoveryKey,
		passphrases:       []string{recoveryKey.String()},
		recoveryKeyTries:  1,
		keyData:           keyData,
		sdCryptsetupCalls: 1})
}

func (s *cryptSuite) TestActivateVolumeWithKeyDataErrorHandling3(c *C) {
	// Test that recovery fallback works when the platform device isn't properly initialized
	keyData, key, _ := s.newNamedKeyData(c, &KeyID{})
	recoveryKey := s.newRecoveryKey()

	s.handler.state = mockPlatformDeviceStateUninitialized

	s.testActivateVolumeWithKeyDataErrorHandling(c, &testActivateVolumeWithKeyDataErrorHandlingData{
		primaryKey:        key,
		recoveryKey:       recoveryKey,
		passphrases:       []string{recoveryKey.String()},
		recoveryKeyTries:  1,
		keyData:           keyData,
		sdCryptsetupCalls: 1})
}

func (s *cryptSuite) TestActivateVolumeWithKeyDataErrorHandling4(c *C) {
	// Test that recovery fallback works when the recovered key is incorrect
	keyData, _, _ := s.newNamedKeyData(c, &KeyID{})
	recoveryKey := s.newRecoveryKey()

	s.testActivateVolumeWithKeyDataErrorHandling(c, &testActivateVolumeWithKeyDataErrorHandlingData{
		recoveryKey:       recoveryKey,
		passphrases:       []string{recoveryKey.String()},
		recoveryKeyTries:  1,
		keyData:           keyData,
		sdCryptsetupCalls: 2})
}

func (s *cryptSuite) TestActivateVolumeWithKeyDataErrorHandling5(c *C) {
	// Test that activation fails if RecoveryKeyTries is zero.
	keyData, key, _ := s.newNamedKeyData(c, &KeyID{Name: "foo", Revision: 2})
	recoveryKey := s.newRecoveryKey()

	s.handler.state = mockPlatformDeviceStateUnavailable

	s.testActivateVolumeWithKeyDataErrorHandling(c, &testActivateVolumeWithKeyDataErrorHandlingData{
		primaryKey:       key,
		recoveryKey:      recoveryKey,
		recoveryKeyTries: 0,
		keyData:          keyData,
		errChecker:       ErrorMatches,
		errCheckerArgs: []interface{}{"cannot activate with platform protected keys:\n" +
			"- foo@2: cannot recover key: the platform's secure device is unavailable: the " +
			"platform device is unavailable\n" +
			"and activation with recovery key failed: no recovery key tries permitted"},
		sdCryptsetupCalls: 0})
}

func (s *cryptSuite) TestActivateVolumeWithKeyDataErrorHandling6(c *C) {
	// Test that activation fails if the supplied recovery key is incorrect
	keyData, key, _ := s.newNamedKeyData(c, &KeyID{Name: "bar", Revision: 5})
	recoveryKey := s.newRecoveryKey()

	s.handler.state = mockPlatformDeviceStateUnavailable

	s.testActivateVolumeWithKeyDataErrorHandling(c, &testActivateVolumeWithKeyDataErrorHandlingData{
		primaryKey:       key,
		recoveryKey:      recoveryKey,
		passphrases:      []string{"00000-00000-00000-00000-00000-00000-00000-00000"},
		recoveryKeyTries: 1,
		keyData:          keyData,
		errChecker:       ErrorMatches,
		errCheckerArgs: []interface{}{"cannot activate with platform protected keys:\n" +
			"- bar@5: cannot recover key: the platform's secure device is unavailable: the " +
			"platform device is unavailable\n" +
			"and activation with recovery key failed: cannot activate volume: " +
			s.mockSdCryptsetup.Exe() + " failed: exit status 5"},
		sdCryptsetupCalls: 1})
}

func (s *cryptSuite) TestActivateVolumeWithKeyDataErrorHandling7(c *C) {
	// Test that recovery fallback works if the correct key is eventually supplied
	keyData, key, _ := s.newNamedKeyData(c, &KeyID{})
	recoveryKey := s.newRecoveryKey()

	s.handler.state = mockPlatformDeviceStateUnavailable

	s.testActivateVolumeWithKeyDataErrorHandling(c, &testActivateVolumeWithKeyDataErrorHandlingData{
		primaryKey:  key,
		recoveryKey: recoveryKey,
		passphrases: []string{
			"00000-00000-00000-00000-00000-00000-00000-00000",
			recoveryKey.String()},
		recoveryKeyTries:  2,
		keyData:           keyData,
		sdCryptsetupCalls: 2})
}

func (s *cryptSuite) TestActivateVolumeWithKeyDataErrorHandling8(c *C) {
	// Test that recovery fallback works if the correct key is eventually supplied
	keyData, key, _ := s.newNamedKeyData(c, &KeyID{})
	recoveryKey := s.newRecoveryKey()

	s.handler.state = mockPlatformDeviceStateUnavailable

	s.testActivateVolumeWithKeyDataErrorHandling(c, &testActivateVolumeWithKeyDataErrorHandlingData{
		primaryKey:  key,
		recoveryKey: recoveryKey,
		passphrases: []string{
			"1234",
			recoveryKey.String()},
		recoveryKeyTries:  2,
		keyData:           keyData,
		sdCryptsetupCalls: 1})
}

type testActivateVolumeWithMultipleKeyDataData struct {
	keys    []DiskUnlockKey
	keyData []*KeyData

	volumeName       string
	sourceDevicePath string
	keyringPrefix    string

	model      SnapModel
	authorized bool

	sdCryptsetupCalls int

	key    DiskUnlockKey
	auxKey AuxiliaryKey
}

func (s *cryptSuite) testActivateVolumeWithMultipleKeyData(c *C, data *testActivateVolumeWithMultipleKeyDataData) {
	for _, k := range data.keys {
		s.addMockKeyslot(c, k)
	}

	options := &ActivateVolumeOptions{KeyringPrefix: data.keyringPrefix}
	modelChecker, err := ActivateVolumeWithMultipleKeyData(data.volumeName, data.sourceDevicePath, data.keyData, options)
	c.Assert(err, IsNil)

	authorized, err := modelChecker.IsModelAuthorized(data.model)
	c.Check(err, IsNil)
	c.Check(authorized, Equals, data.authorized)

	c.Check(s.mockSdAskPassword.Calls(), HasLen, 0)

	c.Check(s.mockSdCryptsetup.Calls(), HasLen, data.sdCryptsetupCalls)
	for _, call := range s.mockSdCryptsetup.Calls() {
		c.Assert(call, HasLen, 6)
		c.Check(call[0:4], DeepEquals, []string{"systemd-cryptsetup", "attach", data.volumeName, data.sourceDevicePath})
		c.Check(call[4], Matches, filepath.Join(s.dir, filepath.Base(os.Args[0]))+"\\.[0-9]+/fifo")
		c.Check(call[5], Equals, "tries=1")
	}

	// This should be done last because it may fail in some circumstances.
	s.checkKeyDataKeysInKeyring(c, data.keyringPrefix, data.sourceDevicePath, data.key, data.auxKey)
}

func (s *cryptSuite) TestActivateVolumeWithMultipleKeyData1(c *C) {
	keyData, keys, auxKeys := s.newMultipleNamedKeyData(c, []*KeyID{&KeyID{}, &KeyID{}})

	models := []SnapModel{
		s.makeMockCore20ModelAssertion(c, map[string]interface{}{
			"authority-id": "fake-brand",
			"series":       "16",
			"brand-id":     "fake-brand",
			"model":        "fake-model",
			"grade":        "secured",
		}, "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij")}
	c.Check(keyData[0].SetAuthorizedSnapModels(auxKeys[0], models...), IsNil)

	s.testActivateVolumeWithMultipleKeyData(c, &testActivateVolumeWithMultipleKeyDataData{
		keys:              keys,
		keyData:           keyData,
		volumeName:        "data",
		sourceDevicePath:  "/dev/sda1",
		model:             models[0],
		authorized:        true,
		sdCryptsetupCalls: 1,
		key:               keys[0],
		auxKey:            auxKeys[0]})
}

func (s *cryptSuite) TestActivateVolumeWithMultipleKeyData2(c *C) {
	// Test with a different volumeName / sourceDevicePath
	keyData, keys, auxKeys := s.newMultipleNamedKeyData(c, []*KeyID{&KeyID{}, &KeyID{}})

	models := []SnapModel{
		s.makeMockCore20ModelAssertion(c, map[string]interface{}{
			"authority-id": "fake-brand",
			"series":       "16",
			"brand-id":     "fake-brand",
			"model":        "fake-model",
			"grade":        "secured",
		}, "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij")}
	c.Check(keyData[0].SetAuthorizedSnapModels(auxKeys[0], models...), IsNil)

	s.testActivateVolumeWithMultipleKeyData(c, &testActivateVolumeWithMultipleKeyDataData{
		keys:              keys,
		keyData:           keyData,
		volumeName:        "foo",
		sourceDevicePath:  "/dev/vda2",
		model:             models[0],
		authorized:        true,
		sdCryptsetupCalls: 1,
		key:               keys[0],
		auxKey:            auxKeys[0]})
}

func (s *cryptSuite) TestActivateVolumeWithMultipleKeyData3(c *C) {
	// Try with an invalid first key - the second key should be used for activation.
	keyData, keys, auxKeys := s.newMultipleNamedKeyData(c, []*KeyID{&KeyID{}, &KeyID{}})

	models := []SnapModel{
		s.makeMockCore20ModelAssertion(c, map[string]interface{}{
			"authority-id": "fake-brand",
			"series":       "16",
			"brand-id":     "fake-brand",
			"model":        "fake-model",
			"grade":        "secured",
		}, "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij")}
	c.Check(keyData[1].SetAuthorizedSnapModels(auxKeys[1], models...), IsNil)

	s.testActivateVolumeWithMultipleKeyData(c, &testActivateVolumeWithMultipleKeyDataData{
		keys:              keys[1:],
		keyData:           keyData,
		volumeName:        "data",
		sourceDevicePath:  "/dev/sda1",
		model:             models[0],
		authorized:        true,
		sdCryptsetupCalls: 2,
		key:               keys[1],
		auxKey:            auxKeys[1]})
}

type testActivateVolumeWithMultipleKeyDataErrorHandlingData struct {
	keys        []DiskUnlockKey
	recoveryKey RecoveryKey
	keyData     []*KeyData

	passphrases []string

	recoveryKeyTries int
	keyringPrefix    string

	errChecker     Checker
	errCheckerArgs []interface{}

	sdCryptsetupCalls int
}

func (s *cryptSuite) testActivateVolumeWithMultipleKeyDataErrorHandling(c *C, data *testActivateVolumeWithMultipleKeyDataErrorHandlingData) {
	for _, key := range data.keys {
		s.addMockKeyslot(c, key)
	}
	s.addMockKeyslot(c, data.recoveryKey[:])

	s.addTryPassphrases(c, data.passphrases)

	options := &ActivateVolumeOptions{
		RecoveryKeyTries: data.recoveryKeyTries,
		KeyringPrefix:    data.keyringPrefix}
	modelChecker, err := ActivateVolumeWithMultipleKeyData("data", "/dev/sda1", data.keyData, options)
	c.Check(modelChecker, IsNil)
	if data.errChecker != nil {
		c.Check(err, data.errChecker, data.errCheckerArgs...)
	} else {
		c.Check(err, IsNil)
	}

	c.Check(s.mockSdAskPassword.Calls(), HasLen, len(data.passphrases))
	for _, call := range s.mockSdAskPassword.Calls() {
		c.Check(call, DeepEquals, []string{"systemd-ask-password", "--icon", "drive-harddisk", "--id",
			filepath.Base(os.Args[0]) + ":/dev/sda1", "Please enter the recovery key for disk /dev/sda1:"})
	}

	c.Check(s.mockSdCryptsetup.Calls(), HasLen, data.sdCryptsetupCalls)
	for _, call := range s.mockSdCryptsetup.Calls() {
		c.Assert(call, HasLen, 6)
		c.Check(call[0:4], DeepEquals, []string{"systemd-cryptsetup", "attach", "data", "/dev/sda1"})
		c.Check(call[4], Matches, filepath.Join(s.dir, filepath.Base(os.Args[0]))+"\\.[0-9]+/fifo")
		c.Check(call[5], Equals, "tries=1")
	}

	if data.errChecker != nil {
		return
	}

	// This should be done last because it may fail in some circumstances.
	s.checkRecoveryKeyInKeyring(c, data.keyringPrefix, "/dev/sda1", data.recoveryKey)
}

func (s *cryptSuite) TestActivateVolumeWithMultipleKeyDataErrorHandling1(c *C) {
	// Test with an invalid value for RecoveryKeyTries.
	keyData, _, _ := s.newMultipleNamedKeyData(c, []*KeyID{&KeyID{}, &KeyID{}})

	s.testActivateVolumeWithMultipleKeyDataErrorHandling(c, &testActivateVolumeWithMultipleKeyDataErrorHandlingData{
		keyData:          keyData,
		recoveryKeyTries: -1,
		errChecker:       ErrorMatches,
		errCheckerArgs:   []interface{}{"invalid RecoveryKeyTries"}})
}

func (s *cryptSuite) TestActivateVolumeWithMultipleKeyDataErrorHandling2(c *C) {
	// Test that recovery fallback works with the platform is unavailable
	keyData, keys, _ := s.newMultipleNamedKeyData(c, []*KeyID{&KeyID{}, &KeyID{}})
	recoveryKey := s.newRecoveryKey()

	s.handler.state = mockPlatformDeviceStateUnavailable

	s.testActivateVolumeWithMultipleKeyDataErrorHandling(c, &testActivateVolumeWithMultipleKeyDataErrorHandlingData{
		keys:              keys,
		recoveryKey:       recoveryKey,
		keyData:           keyData,
		passphrases:       []string{recoveryKey.String()},
		recoveryKeyTries:  1,
		sdCryptsetupCalls: 1})
}

func (s *cryptSuite) TestActivateVolumeWithMultipleKeyDataErrorHandling3(c *C) {
	// Test that recovery fallback works when the platform device isn't properly initialized
	keyData, keys, _ := s.newMultipleNamedKeyData(c, []*KeyID{&KeyID{}, &KeyID{}})
	recoveryKey := s.newRecoveryKey()

	s.handler.state = mockPlatformDeviceStateUninitialized

	s.testActivateVolumeWithMultipleKeyDataErrorHandling(c, &testActivateVolumeWithMultipleKeyDataErrorHandlingData{
		keys:              keys,
		recoveryKey:       recoveryKey,
		keyData:           keyData,
		passphrases:       []string{recoveryKey.String()},
		recoveryKeyTries:  1,
		sdCryptsetupCalls: 1})
}

func (s *cryptSuite) TestActivateVolumeWithMultipleKeyDataErrorHandling4(c *C) {
	// Test that recovery fallback works when the recovered key is incorrect
	keyData, _, _ := s.newMultipleNamedKeyData(c, []*KeyID{&KeyID{}, &KeyID{}})
	recoveryKey := s.newRecoveryKey()

	s.testActivateVolumeWithMultipleKeyDataErrorHandling(c, &testActivateVolumeWithMultipleKeyDataErrorHandlingData{
		recoveryKey:       recoveryKey,
		keyData:           keyData,
		passphrases:       []string{recoveryKey.String()},
		recoveryKeyTries:  1,
		sdCryptsetupCalls: 3})
}

func (s *cryptSuite) TestActivateVolumeWithMultipleKeyDataErrorHandling5(c *C) {
	// Test that activation fails if RecoveryKeyTries is zero.
	keyData, keys, _ := s.newMultipleNamedKeyData(c, []*KeyID{&KeyID{Name: "foo", Revision: 2}, &KeyID{Name: "bar", Revision: 7}})
	recoveryKey := s.newRecoveryKey()

	s.handler.state = mockPlatformDeviceStateUnavailable

	s.testActivateVolumeWithMultipleKeyDataErrorHandling(c, &testActivateVolumeWithMultipleKeyDataErrorHandlingData{
		keys:             keys,
		recoveryKey:      recoveryKey,
		keyData:          keyData,
		recoveryKeyTries: 0,
		errChecker:       ErrorMatches,
		errCheckerArgs: []interface{}{"cannot activate with platform protected keys:\n" +
			"- foo@2: cannot recover key: the platform's secure device is unavailable: the " +
			"platform device is unavailable\n" +
			"- bar@7: cannot recover key: the platform's secure device is unavailable: the " +
			"platform device is unavailable\n" +
			"and activation with recovery key failed: no recovery key tries permitted"},
		sdCryptsetupCalls: 0})
}

func (s *cryptSuite) TestActivateVolumeWithMultipleKeyDataErrorHandling6(c *C) {
	// Test that activation fails if the supplied recovery key is incorrect
	keyData, keys, _ := s.newMultipleNamedKeyData(c, []*KeyID{&KeyID{Name: "bar", Revision: 9}, &KeyID{Name: "foo", Revision: 3}})
	recoveryKey := s.newRecoveryKey()

	s.handler.state = mockPlatformDeviceStateUnavailable

	s.testActivateVolumeWithMultipleKeyDataErrorHandling(c, &testActivateVolumeWithMultipleKeyDataErrorHandlingData{
		keys:             keys,
		recoveryKey:      recoveryKey,
		keyData:          keyData,
		passphrases:      []string{"00000-00000-00000-00000-00000-00000-00000-00000"},
		recoveryKeyTries: 1,
		errChecker:       ErrorMatches,
		errCheckerArgs: []interface{}{"cannot activate with platform protected keys:\n" +
			"- bar@9: cannot recover key: the platform's secure device is unavailable: the " +
			"platform device is unavailable\n" +
			"- foo@3: cannot recover key: the platform's secure device is unavailable: the " +
			"platform device is unavailable\n" +
			"and activation with recovery key failed: cannot activate volume: " +
			s.mockSdCryptsetup.Exe() + " failed: exit status 5"},
		sdCryptsetupCalls: 1})
}

func (s *cryptSuite) TestActivateVolumeWithMultipleKeyDataErrorHandling7(c *C) {
	// Test that recovery fallback works if the correct key is eventually supplied
	keyData, keys, _ := s.newMultipleNamedKeyData(c, []*KeyID{&KeyID{}, &KeyID{}})
	recoveryKey := s.newRecoveryKey()

	s.handler.state = mockPlatformDeviceStateUnavailable

	s.testActivateVolumeWithMultipleKeyDataErrorHandling(c, &testActivateVolumeWithMultipleKeyDataErrorHandlingData{
		keys:        keys,
		recoveryKey: recoveryKey,
		keyData:     keyData,
		passphrases: []string{
			"00000-00000-00000-00000-00000-00000-00000-00000",
			recoveryKey.String()},
		recoveryKeyTries:  2,
		sdCryptsetupCalls: 2})
}

func (s *cryptSuite) TestActivateVolumeWithMultipleKeyDataErrorHandling8(c *C) {
	// Test that recovery fallback works if the correct key is eventually supplied
	keyData, keys, _ := s.newMultipleNamedKeyData(c, []*KeyID{&KeyID{}, &KeyID{}})
	recoveryKey := s.newRecoveryKey()

	s.handler.state = mockPlatformDeviceStateUnavailable

	s.testActivateVolumeWithMultipleKeyDataErrorHandling(c, &testActivateVolumeWithMultipleKeyDataErrorHandlingData{
		keys:        keys,
		recoveryKey: recoveryKey,
		keyData:     keyData,
		passphrases: []string{
			"1234",
			recoveryKey.String()},
		recoveryKeyTries:  2,
		sdCryptsetupCalls: 1})
}

type testActivateVolumeWithKeyData struct {
	activateOptions []string
	keyData         []byte
	expectedKeyData []byte
	errMatch        string
	cmdCalled       bool
}

func (s *cryptSuite) testActivateVolumeWithKey(c *C, data *testActivateVolumeWithKeyData) {
	c.Assert(data.keyData, NotNil)

	expectedKeyData := data.expectedKeyData
	if expectedKeyData == nil {
		expectedKeyData = data.keyData
	}
	s.addMockKeyslot(c, expectedKeyData)

	options := ActivateVolumeOptions{
		ActivateOptions: data.activateOptions,
	}
	err := ActivateVolumeWithKey("luks-volume", "/dev/sda1", data.keyData, &options)
	if data.errMatch == "" {
		c.Check(err, IsNil)
	} else {
		c.Check(err, ErrorMatches, data.errMatch)
	}

	if data.cmdCalled {
		c.Check(len(s.mockSdAskPassword.Calls()), Equals, 0)
		c.Assert(len(s.mockSdCryptsetup.Calls()), Equals, 1)
		c.Assert(len(s.mockSdCryptsetup.Calls()[0]), Equals, 6)

		c.Check(s.mockSdCryptsetup.Calls()[0][0:4], DeepEquals, []string{
			"systemd-cryptsetup", "attach", "luks-volume", "/dev/sda1",
		})
		c.Check(s.mockSdCryptsetup.Calls()[0][4], Matches,
			filepath.Join(s.dir, filepath.Base(os.Args[0]))+"\\.[0-9]+/fifo")
		c.Check(s.mockSdCryptsetup.Calls()[0][5], Equals,
			strings.Join(append(data.activateOptions, "tries=1"), ","))
	} else {
		c.Check(len(s.mockSdAskPassword.Calls()), Equals, 0)
		c.Assert(len(s.mockSdCryptsetup.Calls()), Equals, 0)
	}
}

func (s *cryptSuite) TestActivateVolumeWithKeyNoOptions(c *C) {
	s.testActivateVolumeWithKey(c, &testActivateVolumeWithKeyData{
		activateOptions: nil,
		keyData:         []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		cmdCalled:       true,
	})
}

func (s *cryptSuite) TestActivateVolumeWithKeyWithOptions(c *C) {
	s.testActivateVolumeWithKey(c, &testActivateVolumeWithKeyData{
		activateOptions: []string{"--option"},
		keyData:         []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		cmdCalled:       true,
	})
}

func (s *cryptSuite) TestActivateVolumeWithKeyMismatchErr(c *C) {
	s.testActivateVolumeWithKey(c, &testActivateVolumeWithKeyData{
		activateOptions: []string{"--option"},
		keyData:         []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		expectedKeyData: []byte{0, 0, 0, 0, 1},
		errMatch:        ".*/systemd-cryptsetup failed: exit status 5",
		cmdCalled:       true,
	})
}

func (s *cryptSuite) TestActivateVolumeWithKeyTriesErr(c *C) {
	s.testActivateVolumeWithKey(c, &testActivateVolumeWithKeyData{
		activateOptions: []string{"tries=123"},
		keyData:         []byte{0, 0, 0, 0, 1},
		errMatch:        `cannot specify the "tries=" option for systemd-cryptsetup`,
		cmdCalled:       false,
	})
}

type testInitializeLUKS2ContainerData struct {
	devicePath string
	label      string
	key        []byte
	opts       *InitializeLUKS2ContainerOptions
	formatArgs []string
}

func (s *cryptSuite) testInitializeLUKS2Container(c *C, data *testInitializeLUKS2ContainerData) {
	c.Check(InitializeLUKS2Container(data.devicePath, data.label, data.key, data.opts), IsNil)
	formatArgs := []string{"cryptsetup",
		"-q", "luksFormat", "--type", "luks2",
		"--key-file", "-", "--cipher", "aes-xts-plain64",
		"--key-size", "512",
		"--pbkdf", "argon2i", "--iter-time", "100",
		"--label", data.label, data.devicePath,
	}
	if data.formatArgs != nil {
		formatArgs = data.formatArgs
	}
	c.Check(s.mockCryptsetup.Calls(), DeepEquals, [][]string{
		formatArgs,
		{"cryptsetup", "config", "--priority", "prefer", "--key-slot", "0", data.devicePath}})
	key, err := ioutil.ReadFile(s.cryptsetupKey + ".1")
	c.Assert(err, IsNil)
	c.Check(key, DeepEquals, data.key)
}

func (s *cryptSuite) TestInitializeLUKS2Container1(c *C) {
	s.testInitializeLUKS2Container(c, &testInitializeLUKS2ContainerData{
		devicePath: "/dev/sda1",
		label:      "data",
		key:        s.newPrimaryKey(),
	})
}

func (s *cryptSuite) TestInitializeLUKS2Container2(c *C) {
	// Test with different args.
	s.testInitializeLUKS2Container(c, &testInitializeLUKS2ContainerData{
		devicePath: "/dev/vdc2",
		label:      "test",
		key:        s.newPrimaryKey(),
	})
}

func (s *cryptSuite) TestInitializeLUKS2Container(c *C) {
	// Test with a different key
	s.testInitializeLUKS2Container(c, &testInitializeLUKS2ContainerData{
		devicePath: "/dev/vdc2",
		label:      "test",
		key:        make([]byte, 32),
	})
}

func (s *cryptSuite) TestInitializeLUKS2ContainerWithOptions(c *C) {
	// Test with a different key
	s.testInitializeLUKS2Container(c, &testInitializeLUKS2ContainerData{
		devicePath: "/dev/vdc2",
		label:      "test",
		key:        s.newPrimaryKey(),
		opts: &InitializeLUKS2ContainerOptions{
			MetadataKiBSize:     2 * 1024, // 2MiB
			KeyslotsAreaKiBSize: 3 * 1024, // 3MiB

		},
		formatArgs: []string{"cryptsetup",
			"-q", "luksFormat", "--type", "luks2",
			"--key-file", "-", "--cipher", "aes-xts-plain64",
			"--key-size", "512",
			"--pbkdf", "argon2i", "--iter-time", "100",
			"--label", "test",
			"--luks2-metadata-size", "2048k",
			"--luks2-keyslots-size", "3072k",
			"/dev/vdc2",
		},
	})
}

func (s *cryptSuite) TestInitializeLUKS2ContainerInvalidKeySize(c *C) {
	c.Check(InitializeLUKS2Container("/dev/sda1", "data", s.newPrimaryKey()[0:16], nil), ErrorMatches, "expected a key length of at least 256-bits \\(got 128\\)")
}

func (s *cryptSuite) TestInitializeLUKS2ContainerMetadataKiBSize(c *C) {
	key := make([]byte, 32)
	for _, validSz := range []int{0, 16, 32, 64, 128, 256, 512, 1024, 2048, 4096} {
		opts := InitializeLUKS2ContainerOptions{
			MetadataKiBSize: validSz,
		}
		c.Check(InitializeLUKS2Container("/dev/sda1", "data", key, &opts), IsNil)
	}

	for _, invalidSz := range []int{1, 16 + 3, 8192, 500} {
		opts := InitializeLUKS2ContainerOptions{
			MetadataKiBSize: invalidSz,
		}
		c.Check(InitializeLUKS2Container("/dev/sda1", "data", key, &opts), ErrorMatches,
			fmt.Sprintf("cannot set metadata size to %v KiB", invalidSz))
	}
}

func (s *cryptSuite) TestInitializeLUKS2ContainerKeyslotsSize(c *C) {
	key := make([]byte, 32)
	for _, validSz := range []int{0, 4,
		128 * 1024,
		8 * 1024,
		16,
		256,
	} {
		opts := InitializeLUKS2ContainerOptions{
			KeyslotsAreaKiBSize: validSz,
		}
		c.Check(InitializeLUKS2Container("/dev/sda1", "data", key, &opts), IsNil)
	}

	for _, invalidSz := range []int{
		// smaller than 4096 (4KiB)
		1, 3,
		// misaligned
		40 + 1,
		// larger than 128MB
		128*1024 + 4,
	} {
		opts := InitializeLUKS2ContainerOptions{
			KeyslotsAreaKiBSize: invalidSz,
		}
		c.Check(InitializeLUKS2Container("/dev/sda1", "data", key, &opts), ErrorMatches,
			fmt.Sprintf("cannot set keyslots area size to %v KiB", invalidSz))
	}
}

type testAddRecoveryKeyToLUKS2ContainerData struct {
	devicePath  string
	key         []byte
	recoveryKey RecoveryKey
}

func (s *cryptSuite) testAddRecoveryKeyToLUKS2Container(c *C, data *testAddRecoveryKeyToLUKS2ContainerData) {
	c.Check(AddRecoveryKeyToLUKS2Container(data.devicePath, data.key, data.recoveryKey), IsNil)
	c.Assert(len(s.mockCryptsetup.Calls()), Equals, 1)

	call := s.mockCryptsetup.Calls()[0]
	c.Assert(len(call), Equals, 10)
	c.Check(call[0:3], DeepEquals, []string{"cryptsetup", "luksAddKey", "--key-file"})
	c.Check(call[3], Matches, filepath.Join(s.dir, filepath.Base(os.Args[0]))+"\\.[0-9]+/fifo")
	c.Check(call[4:10], DeepEquals, []string{"--pbkdf", "argon2i", "--iter-time", "5000", data.devicePath, "-"})

	key, err := ioutil.ReadFile(s.cryptsetupKey + ".1")
	c.Assert(err, IsNil)
	c.Check(key, DeepEquals, data.key)

	newKey, err := ioutil.ReadFile(s.cryptsetupNewkey + ".1")
	c.Assert(err, IsNil)
	c.Check(newKey, DeepEquals, data.recoveryKey[:])
}

func (s *cryptSuite) TestAddRecoveryKeyToLUKS2Container1(c *C) {
	s.testAddRecoveryKeyToLUKS2Container(c, &testAddRecoveryKeyToLUKS2ContainerData{
		devicePath:  "/dev/sda1",
		key:         s.newPrimaryKey(),
		recoveryKey: s.newRecoveryKey(),
	})
}

func (s *cryptSuite) TestAddRecoveryKeyToLUKS2Container2(c *C) {
	// Test with different path.
	s.testAddRecoveryKeyToLUKS2Container(c, &testAddRecoveryKeyToLUKS2ContainerData{
		devicePath:  "/dev/vdb2",
		key:         s.newPrimaryKey(),
		recoveryKey: s.newRecoveryKey(),
	})
}

func (s *cryptSuite) TestAddRecoveryKeyToLUKS2Container3(c *C) {
	// Test with different key.
	s.testAddRecoveryKeyToLUKS2Container(c, &testAddRecoveryKeyToLUKS2ContainerData{
		devicePath:  "/dev/vdb2",
		key:         make([]byte, 32),
		recoveryKey: s.newRecoveryKey(),
	})
}

func (s *cryptSuite) TestAddRecoveryKeyToLUKS2Container4(c *C) {
	// Test with different recovery key.
	s.testAddRecoveryKeyToLUKS2Container(c, &testAddRecoveryKeyToLUKS2ContainerData{
		devicePath: "/dev/vdb2",
		key:        s.newPrimaryKey(),
	})
}

type testChangeLUKS2KeyUsingRecoveryKeyData struct {
	devicePath  string
	recoveryKey RecoveryKey
	key         []byte
}

func (s *cryptSuite) testChangeLUKS2KeyUsingRecoveryKey(c *C, data *testChangeLUKS2KeyUsingRecoveryKeyData) {
	c.Check(ChangeLUKS2KeyUsingRecoveryKey(data.devicePath, data.recoveryKey, data.key), IsNil)
	c.Assert(len(s.mockCryptsetup.Calls()), Equals, 3)
	c.Check(s.mockCryptsetup.Calls()[0], DeepEquals, []string{"cryptsetup", "luksKillSlot", "--key-file", "-", data.devicePath, "0"})

	call := s.mockCryptsetup.Calls()[1]
	c.Assert(len(call), Equals, 12)
	c.Check(call[0:3], DeepEquals, []string{"cryptsetup", "luksAddKey", "--key-file"})
	c.Check(call[3], Matches, filepath.Join(s.dir, filepath.Base(os.Args[0]))+"\\.[0-9]+/fifo")
	c.Check(call[4:12], DeepEquals, []string{"--pbkdf", "argon2i", "--iter-time", "100", "--key-slot", "0", data.devicePath, "-"})

	c.Check(s.mockCryptsetup.Calls()[2], DeepEquals, []string{"cryptsetup", "config", "--priority", "prefer", "--key-slot", "0", data.devicePath})

	key, err := ioutil.ReadFile(s.cryptsetupKey + ".1")
	c.Assert(err, IsNil)
	c.Check(key, DeepEquals, data.recoveryKey[:])

	key, err = ioutil.ReadFile(s.cryptsetupKey + ".2")
	c.Assert(err, IsNil)
	c.Check(key, DeepEquals, data.recoveryKey[:])

	key, err = ioutil.ReadFile(s.cryptsetupNewkey + ".2")
	c.Assert(err, IsNil)
	c.Check(key, DeepEquals, data.key)
}

func (s *cryptSuite) TestChangeLUKS2KeyUsingRecoveryKey1(c *C) {
	s.testChangeLUKS2KeyUsingRecoveryKey(c, &testChangeLUKS2KeyUsingRecoveryKeyData{
		devicePath:  "/dev/sda1",
		recoveryKey: s.newRecoveryKey(),
		key:         s.newPrimaryKey(),
	})
}

func (s *cryptSuite) TestChangeLUKS2KeyUsingRecoveryKey2(c *C) {
	s.testChangeLUKS2KeyUsingRecoveryKey(c, &testChangeLUKS2KeyUsingRecoveryKeyData{
		devicePath:  "/dev/vdc1",
		recoveryKey: s.newRecoveryKey(),
		key:         s.newPrimaryKey(),
	})
}

func (s *cryptSuite) TestChangeLUKS2KeyUsingRecoveryKey3(c *C) {
	s.testChangeLUKS2KeyUsingRecoveryKey(c, &testChangeLUKS2KeyUsingRecoveryKeyData{
		devicePath: "/dev/sda1",
		key:        s.newPrimaryKey(),
	})
}

func (s *cryptSuite) TestChangeLUKS2KeyUsingRecoveryKey4(c *C) {
	s.testChangeLUKS2KeyUsingRecoveryKey(c, &testChangeLUKS2KeyUsingRecoveryKeyData{
		devicePath:  "/dev/vdc1",
		recoveryKey: s.newRecoveryKey(),
		key:         make([]byte, 32),
	})
}
