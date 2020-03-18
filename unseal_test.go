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
	"bytes"
	"crypto/rand"
	"io/ioutil"
	"os"
	"testing"

	"github.com/chrisccoulson/go-tpm2"
	. "github.com/snapcore/secboot"
)

var testCreationParams = KeyCreationParams{PCRProfile: getTestPCRProfile(), PinHandle: 0x0181fff0}

func TestUnsealWithNo2FA(t *testing.T) {
	tpm := openTPMForTesting(t)
	defer closeTPM(t, tpm)

	if err := ProvisionTPM(tpm, ProvisionModeFull, nil); err != nil {
		t.Fatalf("Failed to provision TPM for test: %v", err)
	}

	key := make([]byte, 32)
	rand.Read(key)

	tmpDir, err := ioutil.TempDir("", "_TestUnsealWithNo2FA_")
	if err != nil {
		t.Fatalf("Creating temporary directory failed: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	keyFile := tmpDir + "/keydata"

	if err := SealKeyToTPM(tpm, keyFile, "", &testCreationParams, key); err != nil {
		t.Fatalf("SealKeyToTPM failed: %v", err)
	}
	defer undefineKeyNVSpace(t, tpm, keyFile)

	k, err := ReadSealedKeyObject(keyFile)
	if err != nil {
		t.Fatalf("ReadSealedKeyObject failed: %v", err)
	}

	keyUnsealed, err := k.UnsealFromTPM(tpm, "", false)
	if err != nil {
		t.Fatalf("UnsealFromTPM failed: %v", err)
	}

	if !bytes.Equal(key, keyUnsealed) {
		t.Errorf("TPM returned the wrong key")
	}
}

func TestUnsealErrorHandling(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)

	run := func(t *testing.T, tpm *TPMConnection, fn func(string, string)) error {
		if err := ProvisionTPM(tpm, ProvisionModeFull, nil); err != nil {
			t.Errorf("ProvisionTPM failed: %v", err)
		}

		tmpDir, err := ioutil.TempDir("", "_TestUnsealErrorHandling_")
		if err != nil {
			t.Fatalf("Creating temporary directory failed: %v", err)
		}
		defer os.RemoveAll(tmpDir)

		keyFile := tmpDir + "/keydata"
		policyUpdateFile := tmpDir + "/keypolicyupdatedata"

		if err := SealKeyToTPM(tpm, keyFile, policyUpdateFile, &testCreationParams, key); err != nil {
			t.Fatalf("SealKeyToTPM failed: %v", err)
		}
		defer undefineKeyNVSpace(t, tpm, keyFile)

		k, err := ReadSealedKeyObject(keyFile)
		if err != nil {
			t.Fatalf("ReadSealedKeyObject failed: %v", err)
		}

		fn(keyFile, policyUpdateFile)

		_, err = k.UnsealFromTPM(tpm, "", false)
		return err
	}

	t.Run("TPMLockout", func(t *testing.T) {
		tpm := openTPMForTesting(t)
		defer closeTPM(t, tpm)

		err := run(t, tpm, func(_, _ string) {
			// Put the TPM in DA lockout mode
			if err := tpm.DictionaryAttackParameters(tpm.LockoutHandleContext(), 0, 7200, 86400, nil); err != nil {
				t.Errorf("DictionaryAttackParameters failed: %v", err)
			}
		})
		if err != ErrTPMLockout {
			t.Errorf("Unexepcted error: %v", err)
		}
	})

	t.Run("NoSRK", func(t *testing.T) {
		tpm := openTPMForTesting(t)
		defer closeTPM(t, tpm)

		err := run(t, tpm, func(_, _ string) {
			srk, err := tpm.CreateResourceContextFromTPM(SrkHandle)
			if err != nil {
				t.Fatalf("No SRK: %v", err)
			}
			if _, err := tpm.EvictControl(tpm.OwnerHandleContext(), srk, srk.Handle(), nil); err != nil {
				t.Errorf("EvictControl failed: %v", err)
			}
		})
		if err != ErrTPMProvisioning {
			t.Errorf("Unexpected error: %v", err)
		}
	})

	t.Run("InvalidSRK", func(t *testing.T) {
		tpm := openTPMForTesting(t)
		defer closeTPM(t, tpm)

		err := run(t, tpm, func(_, _ string) {
			srk, err := tpm.CreateResourceContextFromTPM(SrkHandle)
			if err != nil {
				t.Fatalf("No SRK: %v", err)
			}
			if _, err := tpm.EvictControl(tpm.OwnerHandleContext(), srk, srk.Handle(), nil); err != nil {
				t.Errorf("EvictControl failed: %v", err)
			}
			srkTemplate := MakeDefaultSRKTemplate()
			srkTemplate.Unique.RSA()[0] = 0xff
			srkTransient, _, _, _, _, err := tpm.CreatePrimary(tpm.OwnerHandleContext(), nil, srkTemplate, nil, nil, nil)
			if err != nil {
				t.Fatalf("CreatePrimary failed: %v", err)
			}
			defer flushContext(t, tpm, srkTransient)
			if _, err := tpm.EvictControl(tpm.OwnerHandleContext(), srkTransient, SrkHandle, nil); err != nil {
				t.Errorf("EvictControl failed: %v", err)
			}
		})
		if _, ok := err.(InvalidKeyFileError); !ok || err.Error() != "invalid key data file: cannot load sealed key object in to TPM: bad "+
			"sealed key object or TPM owner changed" {
			t.Errorf("Unexpected error: %v", err)
		}
	})

	t.Run("IncorrectPCRProfile", func(t *testing.T) {
		tpm, _ := openTPMSimulatorForTesting(t)
		defer closeTPM(t, tpm)

		err := run(t, tpm, func(_, _ string) {
			if _, err := tpm.PCREvent(tpm.PCRHandleContext(7), tpm2.Event("foo"), nil); err != nil {
				t.Errorf("PCREvent failed: %v", err)
			}
		})
		if err == nil {
			t.Fatalf("Expected an error")
		}
		if _, ok := err.(InvalidKeyFileError); !ok || err.Error() != "invalid key data file: cannot complete authorization policy "+
			"assertions: cannot complete OR assertions: current session digest not found in policy data" {
			t.Errorf("Unexpected error: %v", err)
		}
	})

	t.Run("RevokedPolicy", func(t *testing.T) {
		tpm := openTPMForTesting(t)
		defer closeTPM(t, tpm)

		err := run(t, tpm, func(keyFile, policyUpdateFile string) {
			if err := UpdateKeyPCRProtectionPolicy(tpm, keyFile, policyUpdateFile, getTestPCRProfile()); err != nil {
				t.Fatalf("UpdateKeyPCRProtectionPolicy failed: %v", err)
			}
		})
		if _, ok := err.(InvalidKeyFileError); !ok || err.Error() != "invalid key data file: cannot complete authorization policy "+
			"assertions: the dynamic authorization policy has been revoked" {
			t.Errorf("Unexpected error: %v", err)
		}
	})

	t.Run("SealedKeyAccessLocked", func(t *testing.T) {
		tpm := openTPMForTesting(t)
		defer closeTPM(t, tpm)

		err := run(t, tpm, func(keyFile, policyUpdateFile string) {
			lockIndex, err := tpm.CreateResourceContextFromTPM(LockNVHandle)
			if err != nil {
				t.Fatalf("CreateResourceContextFromTPM failed: %v", err)
			}
			if err := tpm.NVReadLock(lockIndex, lockIndex, nil); err != nil {
				t.Errorf("NVReadLock failed: %v", err)
			}
		})
		if err != ErrSealedKeyAccessLocked {
			t.Errorf("Unexpected error: %v", err)
		}
	})
}
